package mcp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/devforth/exception-monitoring/internal/agentprompt"
	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/sentry"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/services"
)

// The tools on projects, issues and their events.

type projectOutput struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	UnresolvedIssues int    `json:"unresolved_issues"`
}

type listProjectsInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Projects per page, 1 to 100 (default 20)."`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

func (handler *Handler) listProjects(ctx context.Context, _ *sdk.CallToolRequest, input listProjectsInput) (*sdk.CallToolResult, any, error) {
	limit, err := parseLimit(input.Limit)

	if err != nil {
		return handler.failed("list_projects", err)
	}

	offset, err := parseCursor(input.Cursor)

	if err != nil {
		return handler.failed("list_projects", err)
	}

	projects := handler.projects.List()
	output := struct {
		Projects   []projectOutput `json:"projects"`
		NextCursor string          `json:"next_cursor,omitempty"`
	}{Projects: []projectOutput{}}

	if offset >= int64(len(projects)) {
		return result(output)
	}

	counts, err := handler.store.UnresolvedCounts(ctx)

	if err != nil {
		return handler.failed("list_projects", err)
	}

	end := min(int(offset)+limit, len(projects))

	for _, project := range projects[int(offset):end] {
		output.Projects = append(output.Projects, projectOutput{ID: project.ID, Name: project.Name, UnresolvedIssues: counts[project.ID]})
	}

	if end < len(projects) {
		output.NextCursor = strconv.Itoa(end)
	}

	return result(output)
}

type listIssuesInput struct {
	Project string `json:"project,omitempty" jsonschema:"Project name or ID; every project when empty."`
	Status  string `json:"status,omitempty" jsonschema:"unresolved (default), resolved, ignored or all."`
	Level   string `json:"level,omitempty" jsonschema:"fatal, error, warning, info or debug."`
	Query   string `json:"query,omitempty" jsonschema:"Text in the issue title or location."`
	Since   string `json:"since,omitempty" jsonschema:"RFC 3339 time, e.g. 2026-11-11T10:00:00Z: only issues with an event at or after it."`
	Until   string `json:"until,omitempty" jsonschema:"RFC 3339 time: only issues with an event before it."`
	Sort    string `json:"sort,omitempty" jsonschema:"last_seen (default), first_seen, event_count or id, newest or most first (append _asc to reverse), or project (A to Z; project_desc reverses)."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Issues per page, 1 to 100 (default 20)."`
	Cursor  string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

type issueOutput struct {
	ID         int64      `json:"id"`
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

func (handler *Handler) issueOutput(issue store.Issue) issueOutput {
	return issueOutput{
		ID: issue.ID, Project: handler.projectName(issue.ProjectID), Title: agentprompt.Redact(issue.Title), Location: agentprompt.Redact(issue.Culprit),
		Level: issue.Level, Status: issue.Status, EventCount: issue.EventCount, FirstSeen: issue.FirstSeen, LastSeen: issue.LastSeen, ResolvedAt: issue.ResolvedAt,
	}
}

func (handler *Handler) listIssues(ctx context.Context, _ *sdk.CallToolRequest, input listIssuesInput) (*sdk.CallToolResult, any, error) {
	filter, err := handler.issueFilter(input)

	if err != nil {
		return handler.failed("list_issues", err)
	}

	limit := filter.Limit
	filter.Limit++ // one extra row tells whether there is a next page
	issues, err := handler.store.ListIssues(ctx, filter)

	if err != nil {
		return handler.failed("list_issues", err)
	}

	issues, more := store.Page(issues, limit)
	output := page[issueOutput]{Items: []issueOutput{}}

	for _, issue := range issues {
		output.Items = append(output.Items, handler.issueOutput(issue))
	}

	if more {
		output.NextCursor = strconv.Itoa(filter.Offset + limit)
	}

	return result(output)
}

func (handler *Handler) issueFilter(input listIssuesInput) (store.IssueFilter, error) {
	filter := store.IssueFilter{Query: strings.TrimSpace(input.Query), Sort: input.Sort}

	var err error

	if filter.ProjectIDs, err = handler.projectIDs(input.Project); err != nil {
		return filter, err
	}

	switch input.Status {
	case "":
		filter.Status = store.StatusUnresolved
	case "all":
	case store.StatusUnresolved, store.StatusResolved, store.StatusIgnored:
		filter.Status = input.Status
	default:
		return filter, invalid("status must be unresolved, resolved, ignored or all")
	}

	if filter.Levels, err = levels(input.Level, config.Levels); err != nil {
		return filter, err
	}

	if !store.ValidIssueSort(filter.Sort) {
		return filter, invalid("sort must be id, last_seen, first_seen or event_count (optionally with _asc), project or project_desc")
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
	IssueID int64  `json:"issue_id" jsonschema:"Issue ID, as list_issues returns it."`
	EventID string `json:"event_id,omitempty" jsonschema:"An event of this issue (see list_issue_events); its latest event when empty."`
}

func (handler *Handler) getIssue(ctx context.Context, _ *sdk.CallToolRequest, input getIssueInput) (*sdk.CallToolResult, any, error) {
	issue, err := handler.store.GetIssue(ctx, input.IssueID)

	if err != nil {
		return handler.failed("get_issue", err)
	}

	event, err := handler.issues.SelectedEvent(ctx, issue, input.EventID)

	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return handler.failed("get_issue", err)
	}

	// Asked for an event the issue does not have: say so rather than
	// describe another one.
	if err != nil && input.EventID != "" {
		return handler.failed("get_issue", fmt.Errorf("event %s of issue %d: %w", input.EventID, issue.ID, errNotFound))
	}

	return handler.describe(ctx, "get_issue", issue, event)
}

type getEventInput struct {
	Project string `json:"project" jsonschema:"Project name or ID."`
	EventID string `json:"event_id" jsonschema:"Event ID: 32 hex characters, with or without dashes."`
}

func (handler *Handler) getEvent(ctx context.Context, _ *sdk.CallToolRequest, input getEventInput) (*sdk.CallToolResult, any, error) {
	project, ok := handler.projects.List().Find(strings.TrimSpace(input.Project))

	if !ok {
		return handler.failed("get_event", invalid("unknown project %q", input.Project))
	}

	event, err := handler.store.GetEvent(ctx, project.ID, sentry.NormalizeEventID(input.EventID))

	if err != nil {
		return handler.failed("get_event", err)
	}

	issue, err := handler.store.GetIssue(ctx, event.IssueID)

	if err != nil {
		return handler.failed("get_event", err)
	}

	return handler.describe(ctx, "get_event", issue, event)
}

// describe writes an issue and one of its events (none when the events are
// gone) as a full agent prompt, after a line of IDs for follow-up calls.
func (handler *Handler) describe(ctx context.Context, tool string, issue store.Issue, event store.Event) (*sdk.CallToolResult, any, error) {
	var (
		parsed      *sentry.Event
		logs        []store.LogRecord
		fromRequest bool
		err         error
	)

	ids := []string{fmt.Sprintf("Issue ID: %d", issue.ID)}

	if event.EventID != "" {
		if parsed, err = sentry.ParseEvent(event.Payload, event.EventID, event.ReceivedAt); err != nil {
			return handler.failed(tool, err)
		}

		if logs, fromRequest, err = handler.issues.RelatedLogsLimit(ctx, event, defaultLimit); err != nil {
			return handler.failed(tool, err)
		}

		ids = append(ids, "Event ID: "+event.EventID)

		if event.TraceID != "" {
			ids = append(ids, "Trace ID: "+event.TraceID)
		}
	} else {
		ids = append(ids, "No stored event: only the issue history is known.")
	}

	if len(logs) == defaultLimit {
		ids = append(ids, "Related logs: limited to 20 recent matches; use filtered list_logs for more.")
	}

	input := services.AgentInput(handler.projectName(issue.ProjectID), issue, parsed, logs, fromRequest)

	return text(strings.Join(ids, "\n") + "\n\n" + agentprompt.Build(input))
}

type listIssueEventsInput struct {
	IssueID int64  `json:"issue_id" jsonschema:"Issue ID."`
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

func (handler *Handler) listIssueEvents(ctx context.Context, _ *sdk.CallToolRequest, input listIssueEventsInput) (*sdk.CallToolResult, any, error) {
	if _, err := handler.store.GetIssue(ctx, input.IssueID); err != nil {
		return handler.failed("list_issue_events", err)
	}

	limit, err := parseLimit(input.Limit)

	if err != nil {
		return handler.failed("list_issue_events", err)
	}

	beforeID, err := parseCursor(input.Cursor)

	if err != nil {
		return handler.failed("list_issue_events", err)
	}

	events, err := handler.store.ListEvents(ctx, store.EventFilter{IssueID: input.IssueID, BeforeID: beforeID, Limit: limit + 1})

	if err != nil {
		return handler.failed("list_issue_events", err)
	}

	events, more := store.Page(events, limit)
	output := page[eventOutput]{Items: []eventOutput{}}

	for _, event := range events {
		output.Items = append(output.Items, eventOutput{
			EventID: event.EventID, Timestamp: event.Timestamp, Level: event.Level,
			Environment: agentprompt.Redact(event.Environment), Release: agentprompt.Redact(event.Release), TraceID: event.TraceID,
		})
	}

	if more {
		output.NextCursor = strconv.FormatInt(events[limit-1].ID, 10)
	}

	return result(output)
}

type getIssuesInput struct {
	IssueIDs []int64 `json:"issue_ids" jsonschema:"1 to 100 known issue IDs. Duplicates are handled once."`
}

type issueBatchOutput struct {
	Issues          []issueOutput `json:"issues"`
	MissingIssueIDs []int64       `json:"missing_issue_ids,omitempty"`
}

func (handler *Handler) batchOutput(ids []int64, issues []store.Issue) issueBatchOutput {
	byID := make(map[int64]store.Issue, len(issues))

	for _, issue := range issues {
		byID[issue.ID] = issue
	}

	output := issueBatchOutput{Issues: []issueOutput{}}

	for _, id := range ids {
		if issue, found := byID[id]; found {
			output.Issues = append(output.Issues, handler.issueOutput(issue))
		} else {
			output.MissingIssueIDs = append(output.MissingIssueIDs, id)
		}
	}

	return output
}

func (handler *Handler) getIssues(ctx context.Context, _ *sdk.CallToolRequest, input getIssuesInput) (*sdk.CallToolResult, any, error) {
	ids, err := store.NormalizeIssueIDs(input.IssueIDs)

	if err != nil {
		return handler.failed("get_issues", invalid("%s", err))
	}

	issues, err := handler.store.GetIssues(ctx, ids)

	if err != nil {
		return handler.failed("get_issues", err)
	}

	return result(handler.batchOutput(ids, issues))
}

type updateIssueStatusInput struct {
	IssueIDs []int64 `json:"issue_ids" jsonschema:"1 to 100 issue IDs; one ID for a single issue."`
	Status   string  `json:"status" jsonschema:"resolved, ignored or unresolved, for every issue in the call."`
}

// updateIssueStatus changes one or many issues at once: all or none.
func (handler *Handler) updateIssueStatus(ctx context.Context, _ *sdk.CallToolRequest, input updateIssueStatusInput) (*sdk.CallToolResult, any, error) {
	ids, err := store.NormalizeIssueIDs(input.IssueIDs)

	if err != nil {
		return handler.failed("update_issue_status", invalid("%s", err))
	}

	switch input.Status {
	case store.StatusResolved, store.StatusIgnored, store.StatusUnresolved:
	default:
		return handler.failed("update_issue_status", invalid("status must be resolved, ignored or unresolved"))
	}

	issues, err := handler.store.SetIssuesStatus(ctx, ids, input.Status)

	if errors.Is(err, store.ErrNotFound) {
		return handler.failed("update_issue_status", invalid("one or more issue IDs were not found; no status changed"))
	}

	if err != nil {
		return handler.failed("update_issue_status", err)
	}

	return result(handler.batchOutput(ids, issues))
}
