// Package contracts holds the data the templates receive: one type per page,
// row or form, with the small display methods templates call. Every view
// type of the UI is defined here and nowhere else; presenters and
// controllers fill them, templates read them. contracts never imports
// controllers, presenters, services or requests.
package contracts

import (
	"time"

	"github.com/devforth/exception-monitoring/internal/sentry"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

type IssueRow struct {
	store.Issue
	ProjectName string
}

// Resolved is when the issue was resolved; zero while it is not (a
// regression clears it).
func (row IssueRow) Resolved() time.Time {
	if row.ResolvedAt == nil {
		return time.Time{}
	}

	return *row.ResolvedAt
}

type IssuesData struct {
	Projects lists.Picker
	Status   string
	Levels   lists.Picker
	Query    string
	Dates    lists.DateRange
	Sort     string
	Filter   lists.FilterLinks
	Issues   []IssueRow
	Pager    lists.Pager
}

// ShowResolved: the "Resolved" column is shown when resolved issues are listed.
func (data IssuesData) ShowResolved() bool {
	return data.Status == store.StatusResolved || data.Status == "all"
}

type IssueData struct {
	Issue  IssueRow
	Events []store.EventSummary
	Event  store.Event
	Parsed *sentry.Event
	// RelatedLogs are the logs of the event's request (LogsFromRequest), or of
	// the same trace around the time of the error when spans do not match.
	RelatedLogs     []store.LogRecord
	LogsFromRequest bool
	// TraceLogs is how many logs the event's trace has.
	TraceLogs   int
	AgentPrompt string
	SelectedID  string
	// History is who changed the status and when, newest first.
	History []store.StatusChange
}

// LastChange is the latest status change, for the header; nil without one.
func (data IssueData) LastChange() *store.StatusChange {
	if len(data.History) == 0 {
		return nil
	}

	return &data.History[0]
}

type LogsData struct {
	Projects lists.Picker
	Query    string
	Dates    lists.DateRange
	TraceID  string
	Levels   lists.Picker
	Filter   lists.FilterLinks
	Logs     []LogRow
	Pager    lists.Pager
}

type LogRow struct {
	store.LogRecord
	ProjectName string
	// Issue is the error raised in the same request, if any.
	Issue *store.LogIssue
}
