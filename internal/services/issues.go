package services

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/devforth/exception-monitoring/internal/api/agentprompt"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Issues: the issue list and pages, the actions on issues and the logs
// shown with an event.
type Issues struct {
	store *store.Store
}

func NewIssues(database *store.Store) *Issues {
	return &Issues{store: database}
}

// Count counts the issues that match filter (its Limit and Offset are ignored).
func (issues *Issues) Count(ctx context.Context, filter store.IssueFilter) (int, error) {
	return issues.store.CountIssues(ctx, filter)
}

// Unresolved counts the unresolved issues of every project: the counter
// next to Issues in the menu.
func (issues *Issues) Unresolved(ctx context.Context) (int, error) {
	return issues.store.CountIssues(ctx, store.IssueFilter{Status: store.StatusUnresolved})
}

// List returns one page of the issues that match filter.
func (issues *Issues) List(ctx context.Context, filter store.IssueFilter) ([]store.Issue, error) {
	return issues.store.ListIssues(ctx, filter)
}

// Get reads an issue; store.ErrNotFound when it does not exist.
func (issues *Issues) Get(ctx context.Context, issueID int64) (store.Issue, error) {
	return issues.store.GetIssue(ctx, issueID)
}

// recentEventsShown is how many events of an issue its page lists.
const recentEventsShown = 20

// RecentEvents lists the newest events of an issue for its page.
func (issues *Issues) RecentEvents(ctx context.Context, issueID int64) ([]store.EventSummary, error) {
	return issues.store.ListEvents(ctx, store.EventFilter{IssueID: issueID, Limit: recentEventsShown})
}

// SelectedEvent is the event the issue page shows: eventID (from ?event=, in
// UUID or hex form) when given, otherwise the latest one. An event of another
// issue is store.ErrNotFound, as a missing one: the page then shows no event
// rather than another issue's.
func (issues *Issues) SelectedEvent(ctx context.Context, issue store.Issue, eventID string) (store.Event, error) {
	if eventID == "" {
		return issues.store.LatestEvent(ctx, issue.ID)
	}

	event, err := issues.store.GetEvent(ctx, issue.ProjectID, sentry.NormalizeEventID(eventID))

	if err == nil && event.IssueID != issue.ID {
		return store.Event{}, store.ErrNotFound
	}

	return event, err
}

// Issue actions are shared by the issue page (one issue) and the issues list
// (the selected issues): both post an "action" and end up in ApplyAction.

// issueActions maps an action to the status it sets; delete sets none.
var issueActions = map[string]string{
	"resolve": store.StatusResolved,
	"ignore":  store.StatusIgnored,
	"reopen":  store.StatusUnresolved,
	"delete":  "",
}

var ErrUnknownAction = errors.New("unknown action")

// ApplyAction runs action on the issues for the viewer, who is recorded in
// the status history; deleting needs CanAdminister. Statuses change in
// batches, one transaction each. An issue that no longer exists (deleted
// from another tab) is skipped, the others are still processed.
func (issues *Issues) ApplyAction(ctx context.Context, viewer store.User, action string, issueIDs []int64) error {
	status, ok := issueActions[action]

	if !ok {
		return ErrUnknownAction
	}

	if action == "delete" && !viewer.CanAdminister() {
		return ErrForbidden
	}

	for batch := range slices.Chunk(issueIDs, store.MaxIssueBatch) {
		var err error

		if action == "delete" {
			err = issues.deleteBatch(ctx, batch)
		} else {
			err = issues.setStatus(ctx, batch, status, viewer.Actor(store.SourceUI))
		}

		if err != nil {
			return fmt.Errorf("%s issues: %w", action, err)
		}
	}

	return nil
}

func (issues *Issues) deleteBatch(ctx context.Context, issueIDs []int64) error {
	for _, issueID := range issueIDs {
		if err := issues.store.DeleteIssue(ctx, issueID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}

	return nil
}

// setStatus changes the issues of a batch that still exist. The batch
// update is all or nothing, so one deleted meanwhile means another try
// without it.
func (issues *Issues) setStatus(ctx context.Context, issueIDs []int64, status string, actor store.Actor) error {
	for {
		existing, err := issues.store.GetIssues(ctx, issueIDs)

		if err != nil || len(existing) == 0 {
			return err
		}

		ids := make([]int64, len(existing))

		for index, issue := range existing {
			ids[index] = issue.ID
		}

		_, err = issues.store.SetIssuesStatus(ctx, ids, status, actor)

		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
}

// History lists the status changes of an issue, newest first.
func (issues *Issues) History(ctx context.Context, issueID int64) ([]store.StatusChange, error) {
	return issues.store.IssueStatusHistory(ctx, issueID)
}

// TraceLogs counts the logs of the event's trace: its trace ID links to
// them only when there are some.
func (issues *Issues) TraceLogs(ctx context.Context, event store.Event) (int, error) {
	if event.TraceID == "" {
		return 0, nil
	}

	return issues.store.CountLogs(ctx, store.LogFilter{TraceID: event.TraceID})
}

// relatedLogsWindow bounds the fallback by time: logs of the same trace this
// close to the error. A browser trace lasts as long as the tab is open.
const relatedLogsWindow = time.Minute

// RelatedLogs returns the logs written around an event, oldest first, and
// whether they are exactly the logs of its request.
//
// A log written during the request carries the span of the route handler (the
// event's span) or of the HTTP request (the event's parent span). Without span
// matches (SDKs that send no spans), it falls back to logs of the same trace
// within relatedLogsWindow of the error.
func (issues *Issues) RelatedLogs(ctx context.Context, event store.Event) ([]store.LogRecord, bool, error) {
	return issues.RelatedLogsLimit(ctx, event, 100)
}

// RelatedLogsLimit bounds diagnostic context at the database query, so MCP
// can start with a smaller sample than the interactive UI.
func (issues *Issues) RelatedLogsLimit(ctx context.Context, event store.Event, limit int) (logs []store.LogRecord, fromRequest bool, err error) {
	limit = min(max(limit, 1), 100)

	if event.TraceID == "" {
		return nil, false, nil
	}

	var spans []string

	for _, span := range []string{event.SpanID, event.ParentSpanID} {
		if span != "" {
			spans = append(spans, span)
		}
	}

	if len(spans) > 0 {
		logs, err = issues.store.ListLogs(ctx, store.LogFilter{TraceID: event.TraceID, SpanIDs: spans, Limit: limit})
		fromRequest = len(logs) > 0
	}

	if err == nil && len(logs) == 0 {
		logs, err = issues.store.ListLogs(ctx, store.LogFilter{
			TraceID: event.TraceID,
			Since:   event.Timestamp.Add(-relatedLogsWindow),
			Until:   event.Timestamp.Add(relatedLogsWindow),
			Limit:   limit,
		})
	}

	// Oldest first: read what happened in order.
	slices.Reverse(logs)

	return logs, fromRequest, err
}

// AgentPrompt is the text of the "Prompt" button: what a coding agent needs
// to explain the error, without what identifies servers or people.
func AgentPrompt(project string, issue store.Issue, event *sentry.Event, relatedLogs []store.LogRecord, fromRequest bool) string {
	return agentprompt.Build(AgentInput(project, issue, event, relatedLogs, fromRequest))
}

// AgentInput is what an agent may learn about an issue: the prompt and the
// MCP server describe the same allowlisted fields.
func AgentInput(project string, issue store.Issue, event *sentry.Event, relatedLogs []store.LogRecord, fromRequest bool) agentprompt.Input {
	logs := make([]agentprompt.Log, 0, len(relatedLogs))

	for _, record := range relatedLogs {
		logs = append(logs, agentprompt.Log{Time: record.Timestamp, Level: record.Level, Body: record.Body})
	}

	return agentprompt.Input{
		Project: project, Title: issue.Title, Level: issue.Level, Culprit: issue.Culprit, Event: event,
		Issue: &agentprompt.Issue{Status: issue.Status, Events: issue.EventCount, FirstSeen: issue.FirstSeen, LastSeen: issue.LastSeen},
		Logs:  logs, LogsFromRequest: fromRequest,
	}
}
