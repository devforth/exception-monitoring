package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/sentry"
	"github.com/devforth/exception-monitoring/internal/store"
)

const pageSize = 50

// Level filters. Sentry events and logs name levels differently: events use
// "warning", logs "warn" and also have "trace".
var (
	eventLevels = []string{"fatal", "error", "warning", "info", "debug"}
	logLevels   = []string{"fatal", "error", "warn", "info", "debug", "trace"}
)

type projectOption struct {
	ID       int
	Name     string
	Selected bool
}

func (handler *Handler) projectOptions(selectedID int) []projectOption {
	options := make([]projectOption, 0, len(handler.projects))

	for _, project := range handler.projects {
		options = append(options, projectOption{ID: project.ID, Name: project.Name, Selected: project.ID == selectedID})
	}

	return options
}

// projectID resolves a project given as an ID or a name; 0 (all projects) if unknown.
func (handler *Handler) projectID(value string) int {
	project, _ := handler.projects.Find(value)

	return project.ID
}

// projectName labels data of a project removed from PROJECTS by its ID.
func (handler *Handler) projectName(projectID int) string {
	if project, ok := handler.projects.ByID(projectID); ok {
		return project.Name
	}

	return fmt.Sprintf("project %d", projectID)
}

// filterLinks builds links that narrow the current list: clicking a project or
// level in a row keeps the other filters and starts from the first page.
type filterLinks struct {
	path  string
	query url.Values
}

func newFilterLinks(r *http.Request) filterLinks {
	return filterLinks{path: r.URL.Path, query: r.URL.Query()}
}

// With returns the current list URL with key set to value: {{.Filter.With "level" .Level}}.
func (links filterLinks) With(key, value string) string {
	query := links.firstPage()
	query.Set(key, value)

	return links.path + "?" + query.Encode()
}

// Current is the list URL with the current filters, from the first page:
// where bulk actions return to.
func (links filterLinks) Current() string {
	return links.path + "?" + links.firstPage().Encode()
}

func (links filterLinks) firstPage() url.Values {
	query := url.Values{}

	for name, values := range links.query {
		if name != "offset" && name != "before" {
			query[name] = values
		}
	}

	return query
}

func (links filterLinks) url(query url.Values) string {
	if len(query) == 0 {
		return links.path
	}

	return links.path + "?" + query.Encode()
}

// filterField describes a list filter for the "active filters" chips.
type filterField struct {
	key, label string
	// unset is the value that means "not filtered" (e.g. the default status).
	unset string
	// names shows a value in words; values not listed are shown as is.
	names map[string]string
	// needs hides the chip unless this other filter is set too.
	needs []string
}

var issueFilterFields = []filterField{
	{key: "project", label: "Project"},
	{key: "status", label: "Status", unset: "unresolved"},
	{key: "level", label: "Level"},
	{key: "query", label: "Search"},
	{key: "from", label: "From"},
	{key: "to", label: "To"},
	{
		key: "date_by", label: "Dates by", unset: "event", needs: []string{"from", "to"},
		names: map[string]string{"first_seen": "first seen", "last_seen": "last seen"},
	},
}

var logFilterFields = []filterField{
	{key: "project", label: "Project"},
	{key: "level", label: "Level"},
	{key: "query", label: "Search"},
	{key: "trace_id", label: "Trace"},
	{key: "from", label: "From"},
	{key: "to", label: "To"},
}

type activeFilter struct {
	Label, Value string
	RemoveURL    string // the list without this filter
}

type activeFilters struct {
	Items    []activeFilter
	ClearURL string // the list without any filter; the sort order stays
}

// active lists the filters applied to the current list, in fields order.
func (links filterLinks) active(fields []filterField) activeFilters {
	result := activeFilters{ClearURL: links.path}

	if sort := links.query.Get("sort"); sort != "" {
		result.ClearURL = links.url(url.Values{"sort": {sort}})
	}

	for _, field := range fields {
		value := strings.TrimSpace(links.query.Get(field.key))

		if value == "" || value == field.unset || !links.hasAny(field.needs) {
			continue
		}

		shown := value

		if name, ok := field.names[value]; ok {
			shown = name
		}

		without := links.firstPage()
		without.Del(field.key)
		result.Items = append(result.Items, activeFilter{Label: field.label, Value: shown, RemoveURL: links.url(without)})
	}

	return result
}

// IssueFilters and LogFilters are the active filters of each list, for templates.
func (links filterLinks) IssueFilters() activeFilters { return links.active(issueFilterFields) }

func (links filterLinks) LogFilters() activeFilters { return links.active(logFilterFields) }

// hasAny reports whether one of keys is set; an empty list means no requirement.
func (links filterLinks) hasAny(keys []string) bool {
	for _, key := range keys {
		if links.query.Get(key) != "" {
			return true
		}
	}

	return len(keys) == 0
}

// dateRange is the ?from= and ?to= filter: days as YYYY-MM-DD in UTC, both
// inclusive. An empty or invalid date means no bound.
type dateRange struct {
	From, To string // as typed, to fill the inputs back
	// Today (UTC) is the latest day the pickers offer: there are no events from the future.
	Today string
	// FromMax is the latest day "from" can be: "to" if set, else today.
	FromMax      string
	Since, Until time.Time
}

func parseDateRange(query url.Values) dateRange {
	dates := dateRange{From: query.Get("from"), To: query.Get("to"), Today: time.Now().UTC().Format(time.DateOnly)}
	from, fromErr := time.Parse(time.DateOnly, dates.From)
	to, toErr := time.Parse(time.DateOnly, dates.To)

	// The date pickers do not allow it, but a typed or edited URL can put "to"
	// before "from". That range matches nothing, so treat it as the reverse.
	if fromErr == nil && toErr == nil && to.Before(from) {
		from, to = to, from
		dates.From, dates.To = dates.To, dates.From
	}

	if fromErr == nil {
		dates.Since = from
	}

	if toErr == nil {
		dates.Until = to.AddDate(0, 0, 1) // the whole "to" day is included
	}

	dates.FromMax = dates.Today

	if dates.To != "" && dates.To < dates.Today {
		dates.FromMax = dates.To
	}

	return dates
}

type issueRow struct {
	store.Issue
	ProjectName string
}

type issuesData struct {
	Projects []projectOption
	Status   string
	Level    string
	Levels   []string
	Query    string
	Dates    dateRange
	DateBy   string // which issue date the range applies to, see store.DateAnyEvent
	Sort     string
	Filter   filterLinks
	Issues   []issueRow
	MoreURL  string // the next page, "" on the last one
}

// issuesPage lists issues. htmx requests get only the part they replace:
// "issue-list" when filters change, "issue-rows" for "load more".
func (handler *Handler) issuesPage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	data := issuesData{
		Status: query.Get("status"),
		Level:  query.Get("level"),
		Levels: eventLevels,
		Query:  query.Get("query"),
		Dates:  parseDateRange(query),
		DateBy: query.Get("date_by"),
		Sort:   query.Get("sort"),
		Filter: newFilterLinks(r),
	}
	projectID := handler.projectID(query.Get("project"))
	data.Projects = handler.projectOptions(projectID)

	if data.Status == "" {
		data.Status = store.StatusUnresolved
	}

	if !store.ValidIssueSort(data.Sort) {
		data.Sort = ""
	}

	if !store.ValidDateField(data.DateBy) {
		data.DateBy = ""
	}

	offset, _ := strconv.Atoi(query.Get("offset"))
	offset = max(offset, 0)

	filter := store.IssueFilter{
		ProjectID: projectID,
		Level:     data.Level,
		Query:     data.Query,
		SeenSince: data.Dates.Since,
		SeenUntil: data.Dates.Until,
		DateField: data.DateBy,
		Sort:      data.Sort,
		Limit:     pageSize + 1,
		Offset:    offset,
	}

	if data.Status != "all" {
		filter.Status = data.Status
	}

	issues, err := handler.store.ListIssues(r.Context(), filter)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	issues, more := store.Page(issues, pageSize)

	if more {
		next := r.URL.Query()
		next.Set("offset", strconv.Itoa(offset+pageSize))
		data.MoreURL = "/issues?" + next.Encode()
	}

	for _, issue := range issues {
		data.Issues = append(data.Issues, issueRow{Issue: issue, ProjectName: handler.projectName(issue.ProjectID)})
	}

	switch {
	case isHTMX(r) && r.Header.Get("HX-Target") == "issue-list":
		handler.renderFragment(w, r, "issues", "issue-list", data)
	case isHTMX(r) && offset > 0:
		handler.renderFragment(w, r, "issues", "issue-rows", data)
	default:
		handler.render(w, r, http.StatusOK, "issues", data)
	}
}

type issueData struct {
	Issue  issueRow
	Events []store.EventSummary
	Event  store.Event
	Parsed *sentry.Event
	// RelatedLogs are the logs of the event's request (LogsFromRequest), or of
	// the same trace around the time of the error when spans do not match.
	RelatedLogs     []store.LogRecord
	LogsFromRequest bool
	SelectedID      string
}

// loadIssue reads the issue from the {issueID} path value. When it fails it
// has already written the response (404 or 500) and returns ok == false.
func (handler *Handler) loadIssue(w http.ResponseWriter, r *http.Request) (issue issueRow, ok bool) {
	issueID, err := strconv.ParseInt(r.PathValue("issueID"), 10, 64)

	if err != nil {
		http.NotFound(w, r)

		return issueRow{}, false
	}

	stored, err := handler.store.GetIssue(r.Context(), issueID)

	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)

		return issueRow{}, false
	}

	if err != nil {
		handler.serverError(w, r, err)

		return issueRow{}, false
	}

	return issueRow{Issue: stored, ProjectName: handler.projectName(stored.ProjectID)}, true
}

// redirectAfterPost sends the browser to target after a form POST. htmx
// would swap a redirect response into the page, so it gets HX-Location
// (a client-side navigation) instead.
func redirectAfterPost(w http.ResponseWriter, r *http.Request, target string) {
	if isHTMX(r) {
		w.Header().Set("HX-Location", target)

		return
	}

	http.Redirect(w, r, target, http.StatusSeeOther)
}

// issuePage shows an issue with one of its events (?event=<event_id>, the
// latest by default): exceptions with stack traces, breadcrumbs, context and
// the logs written during the same request (see relatedLogs).
func (handler *Handler) issuePage(w http.ResponseWriter, r *http.Request) {
	issue, ok := handler.loadIssue(w, r)

	if !ok {
		return
	}

	data := issueData{Issue: issue}

	var err error

	data.Events, err = handler.store.ListEvents(r.Context(), store.EventFilter{IssueID: issue.ID, Limit: 20})

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	if selected := r.URL.Query().Get("event"); selected != "" {
		data.Event, err = handler.store.GetEvent(r.Context(), issue.ProjectID, sentry.NormalizeEventID(selected))

		if err == nil && data.Event.IssueID != issue.ID {
			err = store.ErrNotFound
		}
	} else {
		data.Event, err = handler.store.LatestEvent(r.Context(), issue.ID)
	}

	if err != nil && !errors.Is(err, store.ErrNotFound) {
		handler.serverError(w, r, err)

		return
	}

	if err == nil {
		data.SelectedID = data.Event.EventID
		// Stored payloads were valid when received, so a parse error here is unexpected.
		data.Parsed, err = sentry.ParseEvent(data.Event.Payload, data.Event.EventID, data.Event.ReceivedAt)

		if err != nil {
			handler.logger.Warn("stored event payload does not parse", "event_id", data.Event.EventID, "err", err)
		}

		data.RelatedLogs, data.LogsFromRequest, err = handler.relatedLogs(r.Context(), data.Event)

		if err != nil {
			handler.serverError(w, r, err)

			return
		}
	}

	handler.render(w, r, http.StatusOK, "issue", data)
}

// relatedLogsWindow bounds the fallback by time: logs of the same trace this
// close to the error. A browser trace lasts as long as the tab is open.
const relatedLogsWindow = time.Minute

// relatedLogs returns the logs written around an event, oldest first, and
// whether they are exactly the logs of its request.
//
// A log written during the request carries the span of the route handler (the
// event's span) or of the HTTP request (the event's parent span). Without span
// matches (SDKs that send no spans), it falls back to logs of the same trace
// within relatedLogsWindow of the error.
func (handler *Handler) relatedLogs(ctx context.Context, event store.Event) (logs []store.LogRecord, fromRequest bool, err error) {
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
		logs, err = handler.store.ListLogs(ctx, store.LogFilter{TraceID: event.TraceID, SpanIDs: spans, Limit: 100})
		fromRequest = len(logs) > 0
	}

	if err == nil && len(logs) == 0 {
		logs, err = handler.store.ListLogs(ctx, store.LogFilter{
			TraceID: event.TraceID,
			Since:   event.Timestamp.Add(-relatedLogsWindow),
			Until:   event.Timestamp.Add(relatedLogsWindow),
			Limit:   100,
		})
	}

	// Oldest first: read what happened in order.
	slices.Reverse(logs)

	return logs, fromRequest, err
}

type logsData struct {
	Projects []projectOption
	Level    string
	Query    string
	Dates    dateRange
	TraceID  string
	Levels   []string
	Filter   filterLinks
	Logs     []logRow
	MoreURL  string
}

type logRow struct {
	store.LogRecord
	ProjectName string
	// Issue is the error raised in the same request, if any.
	Issue *store.LogIssue
}

// logsPage searches logs, newest first. Like issuesPage, htmx gets "log-list"
// when filters change and "log-rows" for "load more".
func (handler *Handler) logsPage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	projectID := handler.projectID(query.Get("project"))
	data := logsData{
		Projects: handler.projectOptions(projectID),
		Level:    query.Get("level"),
		Query:    query.Get("query"),
		Dates:    parseDateRange(query),
		TraceID:  strings.TrimSpace(query.Get("trace_id")),
		Levels:   logLevels,
		Filter:   newFilterLinks(r),
	}

	beforeID, _ := strconv.ParseInt(query.Get("before"), 10, 64)

	logs, err := handler.store.ListLogs(r.Context(), store.LogFilter{
		ProjectID: projectID,
		Level:     data.Level,
		Query:     data.Query,
		TraceID:   data.TraceID,
		Since:     data.Dates.Since,
		Until:     data.Dates.Until,
		BeforeID:  max(beforeID, 0),
		Limit:     pageSize + 1,
	})

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	logs, more := store.Page(logs, pageSize)

	if more {
		next := r.URL.Query()
		next.Set("before", strconv.FormatInt(logs[len(logs)-1].ID, 10))
		data.MoreURL = "/logs?" + next.Encode()
	}

	issues, err := handler.store.IssuesForLogs(r.Context(), logs)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	for _, record := range logs {
		row := logRow{LogRecord: record, ProjectName: handler.projectName(record.ProjectID)}

		if issue, ok := issues[record.ID]; ok {
			row.Issue = &issue
		}

		data.Logs = append(data.Logs, row)
	}

	switch {
	case isHTMX(r) && r.Header.Get("HX-Target") == "log-list":
		handler.renderFragment(w, r, "logs", "log-list", data)
	case isHTMX(r) && beforeID > 0:
		handler.renderFragment(w, r, "logs", "log-rows", data)
	default:
		handler.render(w, r, http.StatusOK, "logs", data)
	}
}

type projectInfo struct {
	ID               int
	Name             string
	DSN              string
	UnresolvedIssues int
}

// projectsPage lists projects with the DSN to put into the SDK config. The DSN
// uses the address the admin opened the UI with; behind a proxy that is the
// public address.
func (handler *Handler) projectsPage(w http.ResponseWriter, r *http.Request) {
	counts, err := handler.store.UnresolvedCounts(r.Context())

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	scheme := "http"

	if handler.isHTTPS(r) {
		scheme = "https"
	}

	projects := make([]projectInfo, 0, len(handler.projects))

	for _, project := range handler.projects {
		projects = append(projects, projectInfo{
			ID:               project.ID,
			Name:             project.Name,
			DSN:              fmt.Sprintf("%s://%s@%s/%d", scheme, project.PublicKey, r.Host, project.ID),
			UnresolvedIssues: counts[project.ID],
		})
	}

	handler.render(w, r, http.StatusOK, "projects", projects)
}
