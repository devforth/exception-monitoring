package ui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/agentprompt"
	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/sentry"
	"github.com/devforth/exception-monitoring/internal/store"
)

// The level lists of the filters, highest first.
var (
	eventLevels = highestFirst(config.Levels)
	logLevels   = highestFirst(config.LogLevels)
)

// projectNames lists the current projects for the project filter.
func (handler *Handler) projectNames() []string {
	list := handler.registry.List()
	names := make([]string, 0, len(list))

	for _, project := range list {
		names = append(names, project.Name)
	}

	return names
}

// projectIDs gives the ids of the ?project= names; cleanListQuery has kept
// only known projects, by name, each once. None means all projects.
func (handler *Handler) projectIDs(names []string) []int {
	var ids []int

	for _, name := range names {
		if project, ok := handler.registry.List().Find(name); ok {
			ids = append(ids, project.ID)
		}
	}

	return ids
}

// projectName labels data of a project removed from PROJECTS by its ID.
func (handler *Handler) projectName(projectID int) string {
	if project, ok := handler.registry.List().ByID(projectID); ok {
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

// newFilterLinks builds the links of a list from its cleaned query (see
// cleanListQuery), never from the raw URL.
func newFilterLinks(r *http.Request, query url.Values) filterLinks {
	return filterLinks{path: r.URL.Path, query: query}
}

// With returns the current list URL with key set to value: {{.Filter.With "level" .Level}}.
func (links filterLinks) With(key, value string) string {
	query := links.firstPage()
	query.Set(key, value)

	return links.path + "?" + query.Encode()
}

// Current is the list URL with the current filters and page: where bulk
// actions return to.
func (links filterLinks) Current() string {
	return links.url(links.query)
}

// firstPage is a copy of the query without the page.
func (links filterLinks) firstPage() url.Values {
	query := maps.Clone(links.query)
	delete(query, "page")

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
	// dates: the date range (from, to) as one chip.
	dates bool
	// badges: several values, shown as badges ("project" or "level") in one
	// chip, each removable on its own.
	badges string
}

var issueFilterFields = []filterField{
	{key: "project", label: "Project", badges: "project"},
	{key: "status", label: "Status", unset: "unresolved", badges: "status"},
	{key: "level", label: "Level", badges: "level"},
	{key: "query", label: "Search"},
	{label: "Dates", dates: true},
	{key: "sort", label: "Sort", unset: "last_seen", names: map[string]string{"first_seen": "first seen", "event_count": "most events"}},
}

var logFilterFields = []filterField{
	{key: "project", label: "Project", badges: "project"},
	{key: "level", label: "Level", badges: "level"},
	{key: "query", label: "Search"},
	{key: "trace_id", label: "Trace"},
	{label: "Dates", dates: true},
}

type activeFilter struct {
	Label, Value string
	RemoveURL    string // the list without this filter
	// Badges: several chosen values in one chip, each with its own × (RemoveURL).
	Badges []badgeView
}

type activeFilters struct {
	Items    []activeFilter
	ClearURL string // the list without any filter; the page size stays
}

// active lists the filters applied to the current list, in fields order.
func (links filterLinks) active(fields []filterField) activeFilters {
	// The page size is not a filter: "Clear all" keeps it.
	result := activeFilters{ClearURL: links.path}

	if perPage := links.query.Get("per_page"); perPage != "" {
		result.ClearURL = links.url(url.Values{"per_page": {perPage}})
	}

	for _, field := range fields {
		if field.badges != "" {
			result.Items = append(result.Items, links.badgeChips(field)...)

			continue
		}

		if field.dates {
			result.Items = append(result.Items, links.datesChip(field)...)

			continue
		}

		value := strings.TrimSpace(links.query.Get(field.key))

		if value == "" || value == field.unset {
			continue
		}

		if name, ok := field.names[value]; ok {
			value = name
		}

		without := links.firstPage()
		without.Del(field.key)
		result.Items = append(result.Items, activeFilter{Label: field.label, Value: value, RemoveURL: links.url(without)})
	}

	return result
}

// badgeChips gives one chip with every chosen value; the × of a value
// removes just that one.
func (links filterLinks) badgeChips(field filterField) []activeFilter {
	chip := activeFilter{Label: field.label}
	selected := links.query[field.key]

	// The default (e.g. unresolved issues) is not a filter.
	if field.unset != "" && len(selected) == 1 && selected[0] == field.unset {
		return nil
	}

	for _, value := range selected {
		if strings.TrimSpace(value) == "" {
			continue
		}

		without := links.firstPage()
		without[field.key] = slices.DeleteFunc(slices.Clone(selected), func(other string) bool { return other == value })
		chip.Badges = append(chip.Badges, badgeView{Kind: field.badges, Name: value, RemoveURL: links.url(without)})
	}

	if len(chip.Badges) == 0 {
		return nil
	}

	return []activeFilter{chip}
}

// IssueFilters and LogFilters are the active filters of each list, for templates.
func (links filterLinks) IssueFilters() activeFilters { return links.active(issueFilterFields) }

func (links filterLinks) LogFilters() activeFilters { return links.active(logFilterFields) }

// datesChip shows the date range as one chip ("2026-11-01 – 2026-11-10"),
// and × removes both ends.
func (links filterLinks) datesChip(field filterField) []activeFilter {
	from, to := strings.TrimSpace(links.query.Get("from")), strings.TrimSpace(links.query.Get("to"))

	var value string

	switch {
	case from != "" && to != "":
		value = from + " – " + to
	case from != "":
		value = "from " + from
	case to != "":
		value = "until " + to
	default:
		return nil
	}

	without := links.firstPage()

	for _, key := range []string{"from", "to"} {
		without.Del(key)
	}

	return []activeFilter{{Label: field.label, Value: value, RemoveURL: links.url(without)}}
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

// Resolved is when the issue was resolved; zero while it is not (a
// regression clears it).
func (row issueRow) Resolved() time.Time {
	if row.ResolvedAt == nil {
		return time.Time{}
	}

	return *row.ResolvedAt
}

type issuesData struct {
	Projects pickerView
	Status   string
	Levels   pickerView
	Query    string
	Dates    dateRange
	Sort     string
	Filter   filterLinks
	Issues   []issueRow
	Pager    pager
}

// ShowResolved: the "Resolved" column is shown when resolved issues are listed.
func (data issuesData) ShowResolved() bool {
	return data.Status == store.StatusResolved || data.Status == "all"
}

// issuesPage lists issues. htmx requests (filters, page links) get only the
// "issue-list" part they replace.
func (handler *Handler) issuesPage(w http.ResponseWriter, r *http.Request) {
	query := handler.cleanListQuery(r.URL.Query(), issueListParams)
	data := issuesData{
		Status: query.Get("status"),
		Levels: newPicker("level", "level", "All levels", "Levels", eventLevels, query["level"]),
		Query:  query.Get("query"),
		Dates:  parseDateRange(query),
		Sort:   query.Get("sort"),
		Filter: newFilterLinks(r, query),
	}
	projectIDs := handler.projectIDs(query["project"])
	data.Projects = newPicker("project", "project", "All projects", "Projects", handler.projectNames(), query["project"])

	if data.Status == "" {
		data.Status = store.StatusUnresolved
	}

	page, perPage := readPaging(query)
	filter := store.IssueFilter{
		ProjectIDs: projectIDs,
		Levels:     query["level"],
		Query:      data.Query,
		SeenSince:  data.Dates.Since,
		SeenUntil:  data.Dates.Until,
		Sort:       data.Sort,
	}

	if data.Status != "all" {
		filter.Status = data.Status
	}

	total, err := handler.store.CountIssues(r.Context(), filter)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	// A page past the end (fewer results after a filter or a delete) shows the last one.
	data.Pager = newPager(data.Filter, page, perPage, total, "issue-list")
	filter.Limit, filter.Offset = perPage, (data.Pager.Page-1)*perPage

	issues, err := handler.store.ListIssues(r.Context(), filter)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	for _, issue := range issues {
		data.Issues = append(data.Issues, issueRow{Issue: issue, ProjectName: handler.projectName(issue.ProjectID)})
	}

	switch {
	case isHTMX(r) && r.Header.Get("HX-Target") == "issue-list":
		handler.renderFragment(w, r, "issues", "issue-list", data)
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
	// TraceLogs is how many logs the event's trace has.
	TraceLogs   int
	AgentPrompt string
	SelectedID  string
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

		if err == nil {
			data.TraceLogs, err = handler.traceLogs(r.Context(), data.Event)
		}

		if err != nil {
			handler.serverError(w, r, err)

			return
		}
	}

	data.AgentPrompt = agentPrompt(issue.ProjectName, issue.Issue, data.Parsed, data.RelatedLogs, data.LogsFromRequest)

	handler.render(w, r, http.StatusOK, "issue", data)
}

// traceLogs counts the logs of the event's trace: its trace ID links to
// them only when there are some.
func (handler *Handler) traceLogs(ctx context.Context, event store.Event) (int, error) {
	if event.TraceID == "" {
		return 0, nil
	}

	return handler.store.CountLogs(ctx, store.LogFilter{TraceID: event.TraceID})
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
	Projects pickerView
	Query    string
	Dates    dateRange
	TraceID  string
	Levels   pickerView
	Filter   filterLinks
	Logs     []logRow
	Pager    pager
}

type logRow struct {
	store.LogRecord
	ProjectName string
	// Issue is the error raised in the same request, if any.
	Issue *store.LogIssue
}

// logsPage searches logs, newest first. Like issuesPage, htmx gets only
// "log-list" for filters and page links.
func (handler *Handler) logsPage(w http.ResponseWriter, r *http.Request) {
	query := handler.cleanListQuery(r.URL.Query(), logListParams)
	projectIDs := handler.projectIDs(query["project"])
	data := logsData{
		Projects: newPicker("project", "project", "All projects", "Projects", handler.projectNames(), query["project"]),
		Query:    query.Get("query"),
		Dates:    parseDateRange(query),
		TraceID:  strings.TrimSpace(query.Get("trace_id")),
		Levels:   newPicker("level", "level", "All levels", "Levels", logLevels, query["level"]),
		Filter:   newFilterLinks(r, query),
	}

	page, perPage := readPaging(query)
	filter := store.LogFilter{
		ProjectIDs: projectIDs,
		Levels:     query["level"],
		Query:      data.Query,
		TraceID:    data.TraceID,
		Since:      data.Dates.Since,
		Until:      data.Dates.Until,
	}

	total, err := handler.store.CountLogs(r.Context(), filter)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	data.Pager = newPager(data.Filter, page, perPage, total, "log-list")
	filter.Limit, filter.Offset = perPage, (data.Pager.Page-1)*perPage

	logs, err := handler.store.ListLogs(r.Context(), filter)

	if err != nil {
		handler.serverError(w, r, err)

		return
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
	default:
		handler.render(w, r, http.StatusOK, "logs", data)
	}
}

// agentPrompt is the text of the "Prompt" button: what a coding agent needs
// to fix the error, without what identifies servers or people.
func agentPrompt(project string, issue store.Issue, event *sentry.Event, relatedLogs []store.LogRecord, fromRequest bool) string {
	logs := make([]agentprompt.Log, 0, len(relatedLogs))

	for _, record := range relatedLogs {
		logs = append(logs, agentprompt.Log{Time: record.Timestamp, Level: record.Level, Body: record.Body})
	}

	return agentprompt.Build(agentprompt.Input{
		Project: project, Title: issue.Title, Level: issue.Level, Culprit: issue.Culprit, Event: event,
		Issue: &agentprompt.Issue{Status: issue.Status, Events: issue.EventCount, FirstSeen: issue.FirstSeen, LastSeen: issue.LastSeen},
		Logs:  logs, LogsFromRequest: fromRequest,
	})
}
