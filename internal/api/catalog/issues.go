package catalog

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/api/agentprompt"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

type listIssuesInput struct {
	Project string `json:"project,omitempty" jsonschema:"Project name or ID; every project when empty."`
	Status  string `json:"status,omitempty" jsonschema:"unresolved (default), resolved, ignored or all."`
	Level   string `json:"level,omitempty" jsonschema:"fatal, error, warning, info or debug."`
	Query   string `json:"query,omitempty" jsonschema:"Text in the issue title or location."`
	Since   string `json:"since,omitempty" jsonschema:"RFC 3339 time, e.g. 2026-11-11T10:00:00Z: only issues with an event at or after it."`
	Until   string `json:"until,omitempty" jsonschema:"RFC 3339 time: only issues with an event before it."`
	DateBy  string `json:"date_by,omitempty" jsonschema:"What since/until apply to: event (default: any event of the issue), first_seen or last_seen."`
	Sort    string `json:"sort,omitempty" jsonschema:"last_seen (default), first_seen or event_count, newest or most first (append _asc to reverse), or project (A to Z; project_desc reverses)."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Issues per page, 1 to 100 (default 20)."`
	Cursor  string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

type issueOutput struct {
	ID         string     `json:"id"`
	Project    string     `json:"project"`
	Title      string     `json:"title"`
	Location   string     `json:"location,omitempty"`
	Level      string     `json:"level"`
	Status     string     `json:"status"`
	EventCount int        `json:"event_count"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   time.Time  `json:"last_seen"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

func (handler *handlers) issueOutput(call Call, issue store.Issue) issueOutput {
	return issueOutput{
		ID: issue.ID, Project: handler.projectName(issue.ProjectID), Title: call.Text(issue.Title), Location: call.Text(issue.Culprit),
		Level: issue.Level, Status: issue.Status, EventCount: issue.EventCount, FirstSeen: issue.FirstSeen, LastSeen: issue.LastSeen, ResolvedAt: issue.ResolvedAt,
	}
}

// projectName is empty for data of a deleted project.
func (handler *handlers) projectName(projectID int) string {
	project, _ := handler.projects.List().ByID(projectID)

	return project.Name
}

func (handler *handlers) listIssues(ctx context.Context, call Call, input listIssuesInput) (Page[issueOutput], error) {
	output := Page[issueOutput]{Items: []issueOutput{}}
	filter, err := handler.issueFilter(input)

	if err != nil {
		return output, err
	}

	limit := filter.Limit
	filter.Limit++ // one extra row tells whether there is a next page
	issues, err := handler.store.ListIssues(ctx, filter)

	if err != nil {
		return output, err
	}

	issues, more := store.Page(issues, limit)

	for _, issue := range issues {
		output.Items = append(output.Items, handler.issueOutput(call, issue))
	}

	if more {
		output.NextCursor = strconv.Itoa(filter.Offset + limit)
	}

	return output, nil
}

func (handler *handlers) issueFilter(input listIssuesInput) (store.IssueFilter, error) {
	filter := store.IssueFilter{Query: strings.TrimSpace(input.Query), Sort: input.Sort, DateField: input.DateBy}

	var err error

	if filter.ProjectIDs, err = projectIDs(handler.projects.List(), input.Project); err != nil {
		return filter, err
	}

	switch input.Status {
	case "":
		filter.Status = store.StatusUnresolved
	case "all":
	case store.StatusUnresolved, store.StatusResolved, store.StatusIgnored:
		filter.Status = input.Status
	default:
		return filter, Invalid("status must be unresolved, resolved, ignored or all")
	}

	if filter.Levels, err = levels(input.Level, config.Levels); err != nil {
		return filter, err
	}

	if !store.ValidIssueSort(filter.Sort) {
		return filter, Invalid("sort must be last_seen, first_seen or event_count (optionally with _asc), project or project_desc")
	}

	if !store.ValidDateField(filter.DateField) {
		return filter, Invalid("date_by must be event, first_seen or last_seen")
	}

	if filter.SeenSince, err = parseTime("since", input.Since); err != nil {
		return filter, err
	}

	if filter.SeenUntil, err = parseTime("until", input.Until); err != nil {
		return filter, err
	}

	if filter.Limit, err = parseLimit(input.Limit); err != nil {
		return filter, err
	}

	offset, err := parseCursor(input.Cursor)
	filter.Offset = int(offset)

	return filter, err
}

type getIssueInput struct {
	IssueID string `json:"issue_id" jsonschema:"Issue ID, as list_issues returns it."`
	EventID string `json:"event_id,omitempty" jsonschema:"An event of this issue (see list_issue_events); its latest event when empty."`
}

func (handler *handlers) getIssue(ctx context.Context, _ Call, input getIssueInput) (Text, error) {
	issue, err := handler.store.GetIssue(ctx, input.IssueID)

	if err != nil {
		return "", err
	}

	event, err := handler.services.Issues.SelectedEvent(ctx, issue, input.EventID)

	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}

	// Asked for an event the issue does not have: say so rather than
	// describe another one.
	if err != nil && input.EventID != "" {
		return "", fmt.Errorf("event %s of issue %s: %w", input.EventID, issue.ID, ErrNotFound)
	}

	return handler.describe(ctx, issue, event)
}

type getEventInput struct {
	Project string `json:"project" jsonschema:"Project name or ID."`
	EventID string `json:"event_id" jsonschema:"Event ID: 32 hex characters, with or without dashes."`
}

func (handler *handlers) findEvent(ctx context.Context, input getEventInput) (store.Event, error) {
	project, ok := handler.projects.List().Find(strings.TrimSpace(input.Project))

	if !ok {
		return store.Event{}, Invalid("unknown project %q (see list_projects)", input.Project)
	}

	return handler.store.GetEvent(ctx, project.ID, sentry.NormalizeEventID(input.EventID))
}

func (handler *handlers) getEvent(ctx context.Context, _ Call, input getEventInput) (Text, error) {
	event, err := handler.findEvent(ctx, input)

	if err != nil {
		return "", err
	}

	issue, err := handler.store.GetIssue(ctx, event.IssueID)

	if err != nil {
		return "", err
	}

	return handler.describe(ctx, issue, event)
}

// getEventPayload is REST only: the payload holds everything the SDK sent.
func (handler *handlers) getEventPayload(ctx context.Context, _ Call, input getEventInput) (store.Event, error) {
	return handler.findEvent(ctx, input)
}

// describe writes an issue and one of its events (none when the events are
// gone) as a full agent prompt, after a line of IDs for follow-up calls.
// The prompt is built from an allowlist and redacted (package agentprompt),
// whoever asks.
func (handler *handlers) describe(ctx context.Context, issue store.Issue, event store.Event) (Text, error) {
	var (
		parsed      *sentry.Event
		logs        []store.LogRecord
		fromRequest bool
		err         error
	)

	ids := []string{fmt.Sprintf("Issue ID: %s", issue.ID)}

	if event.EventID != "" {
		if parsed, err = sentry.ParseEvent(event.Payload, event.EventID, event.ReceivedAt); err != nil {
			return "", err
		}

		if logs, fromRequest, err = handler.services.Issues.RelatedLogsLimit(ctx, event, DefaultLimit); err != nil {
			return "", err
		}

		ids = append(ids, "Event ID: "+event.EventID)

		if event.TraceID != "" {
			ids = append(ids, "Trace ID: "+event.TraceID)
		}
	} else {
		ids = append(ids, "No stored event: only the issue history is known.")
	}

	if len(logs) == DefaultLimit {
		ids = append(ids, "Related logs: limited to 20 recent matches; use filtered list_logs for more.")
	}

	input := services.AgentInput(handler.projectName(issue.ProjectID), issue, parsed, logs, fromRequest)

	return Text(strings.Join(ids, "\n") + "\n\n" + agentprompt.Build(input)), nil
}

type listIssueEventsInput struct {
	IssueID string `json:"issue_id" jsonschema:"Issue ID."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Events per page, 1 to 100 (default 20)."`
	Cursor  string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

type eventOutput struct {
	EventID     string    `json:"event_id"`
	Timestamp   time.Time `json:"timestamp"`
	Level       string    `json:"level"`
	Environment string    `json:"environment,omitempty"`
	Release     string    `json:"release,omitempty"`
	TraceID     string    `json:"trace_id,omitempty"`
}

func (handler *handlers) listIssueEvents(ctx context.Context, call Call, input listIssueEventsInput) (Page[eventOutput], error) {
	output := Page[eventOutput]{Items: []eventOutput{}}

	if _, err := handler.store.GetIssue(ctx, input.IssueID); err != nil {
		return output, err
	}

	limit, err := parseLimit(input.Limit)

	if err != nil {
		return output, err
	}

	beforeID, err := parseCursor(input.Cursor)

	if err != nil {
		return output, err
	}

	events, err := handler.store.ListEvents(ctx, store.EventFilter{IssueID: input.IssueID, BeforeID: beforeID, Limit: limit + 1})

	if err != nil {
		return output, err
	}

	events, more := store.Page(events, limit)

	for _, event := range events {
		output.Items = append(output.Items, eventOutput{
			EventID: event.EventID, Timestamp: event.Timestamp, Level: event.Level,
			Environment: call.Text(event.Environment), Release: call.Text(event.Release), TraceID: event.TraceID,
		})
	}

	if more {
		output.NextCursor = strconv.FormatInt(events[limit-1].ID, 10)
	}

	return output, nil
}

type issueIDsInput struct {
	IssueIDs []string `json:"issue_ids" jsonschema:"1 to 100 known issue IDs. Duplicates are handled once."`
}

type issueBatchOutput struct {
	Issues          []issueOutput `json:"issues"`
	MissingIssueIDs []string      `json:"missing_issue_ids,omitempty"`
}

func (handler *handlers) batchOutput(call Call, ids []string, issues []store.Issue) issueBatchOutput {
	byID := make(map[string]store.Issue, len(issues))

	for _, issue := range issues {
		byID[issue.ID] = issue
	}

	output := issueBatchOutput{Issues: []issueOutput{}}

	for _, id := range ids {
		if issue, found := byID[id]; found {
			output.Issues = append(output.Issues, handler.issueOutput(call, issue))
		} else {
			output.MissingIssueIDs = append(output.MissingIssueIDs, id)
		}
	}

	return output
}

func (handler *handlers) getIssues(ctx context.Context, call Call, input issueIDsInput) (issueBatchOutput, error) {
	ids, err := store.NormalizeIssueIDs(input.IssueIDs)

	if err != nil {
		return issueBatchOutput{}, Invalid("%s", err)
	}

	issues, err := handler.store.GetIssues(ctx, ids)

	if err != nil {
		return issueBatchOutput{}, err
	}

	return handler.batchOutput(call, ids, issues), nil
}

type updateIssueStatusInput struct {
	IssueIDs []string `json:"issue_ids" jsonschema:"1 to 100 issue IDs; one ID for a single issue."`
	Status   string   `json:"status" jsonschema:"resolved, ignored or unresolved, for every issue in the call."`
}

// updateIssueStatus changes one or many issues at once: all or none. The
// change is recorded in the issues' history with the call's actor.
func (handler *handlers) updateIssueStatus(ctx context.Context, call Call, input updateIssueStatusInput) (issueBatchOutput, error) {
	ids, err := store.NormalizeIssueIDs(input.IssueIDs)

	if err != nil {
		return issueBatchOutput{}, Invalid("%s", err)
	}

	switch input.Status {
	case store.StatusResolved, store.StatusIgnored, store.StatusUnresolved:
	default:
		return issueBatchOutput{}, Invalid("status must be resolved, ignored or unresolved")
	}

	issues, err := handler.store.SetIssuesStatus(ctx, ids, input.Status, call.Actor)

	if errors.Is(err, store.ErrNotFound) {
		return issueBatchOutput{}, Invalid("one or more issue IDs were not found; no status changed")
	}

	if err != nil {
		return issueBatchOutput{}, err
	}

	return handler.batchOutput(call, ids, issues), nil
}

type deleteIssueInput struct {
	IssueID string `json:"issue_id" jsonschema:"Issue ID."`
}

type deletedOutput struct {
	Deleted string `json:"deleted"`
}

func (handler *handlers) deleteIssue(ctx context.Context, _ Call, input deleteIssueInput) (deletedOutput, error) {
	if deleted, err := handler.store.DeleteIssues(ctx, []string{input.IssueID}); err != nil || deleted == 0 {
		return deletedOutput{}, cmp.Or(err, store.ErrNotFound)
	}

	return deletedOutput{Deleted: input.IssueID}, nil
}
