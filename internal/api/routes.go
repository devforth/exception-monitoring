package api

import (
	"encoding/json"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/httpjson"
	"github.com/devforth/exception-monitoring/internal/sentry"
	"github.com/devforth/exception-monitoring/internal/store"
)

type projectResponse struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	UnresolvedIssues int    `json:"unresolved_issues"`
}

func (handler *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	counts, err := handler.store.UnresolvedCounts(r.Context())

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	list := handler.projects.List()
	projects := make([]projectResponse, 0, len(list))

	for _, project := range list {
		projects = append(projects, projectResponse{ID: project.ID, Name: project.Name, UnresolvedIssues: counts[project.ID]})
	}

	httpjson.Write(w, http.StatusOK, map[string]any{"items": projects})
}

type issueResponse struct {
	store.Issue
	Project string `json:"project"`
}

func (handler *Handler) issueResponse(issue store.Issue) issueResponse {
	return issueResponse{Issue: issue, Project: handler.projectName(issue.ProjectID)}
}

// GET /api/v1/issues?project=&status=&level=&query=&since=&until=&date_by=&sort=&limit=&cursor=
// status defaults to "unresolved"; status=all lists every status.
func (handler *Handler) listIssues(w http.ResponseWriter, r *http.Request) {
	filter, err := handler.issueFilter(r)

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	limit := filter.Limit
	filter.Limit++ // one extra row tells whether there is a next page
	issues, err := handler.store.ListIssues(r.Context(), filter)

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	result := page[issueResponse]{Items: []issueResponse{}}
	issues, more := store.Page(issues, limit)

	if more {
		result.NextCursor = encodeCursor('o', int64(filter.Offset+limit))
	}

	for _, issue := range issues {
		result.Items = append(result.Items, handler.issueResponse(issue))
	}

	httpjson.Write(w, http.StatusOK, result)
}

func (handler *Handler) issueFilter(r *http.Request) (store.IssueFilter, error) {
	query := r.URL.Query()

	var (
		filter store.IssueFilter
		err    error
	)

	if filter.ProjectIDs, err = handler.projectsFromQuery(r); err != nil {
		return filter, err
	}

	switch status := query.Get("status"); status {
	case "":
		filter.Status = store.StatusUnresolved
	case "all":
	case store.StatusUnresolved, store.StatusResolved, store.StatusIgnored:
		filter.Status = status
	default:
		return filter, invalid("status must be unresolved, resolved, ignored or all")
	}

	filter.Levels = levelsFromQuery(query)
	filter.Query = query.Get("query")
	filter.Sort = query.Get("sort")

	if !store.ValidIssueSort(filter.Sort) {
		return filter, invalid("sort must be id, last_seen, first_seen or event_count (optionally with _asc), project or project_desc")
	}

	// since/until bound the date chosen by date_by: "event" (default, any event
	// in [since, until)), "first_seen" or "last_seen".
	if filter.DateField = query.Get("date_by"); !store.ValidDateField(filter.DateField) {
		return filter, invalid("date_by must be event, first_seen or last_seen")
	}

	if filter.SeenSince, err = parseTimeParam(r, "since"); err != nil {
		return filter, err
	}

	if filter.SeenUntil, err = parseTimeParam(r, "until"); err != nil {
		return filter, err
	}

	if filter.Limit, err = parseLimit(r); err != nil {
		return filter, err
	}

	offset, err := decodeCursor(query.Get("cursor"), 'o')
	filter.Offset = int(offset)

	return filter, err
}

func (handler *Handler) getIssue(w http.ResponseWriter, r *http.Request) {
	issueID, err := pathID(r, "issueID")

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	issue, err := handler.store.GetIssue(r.Context(), issueID)

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	httpjson.Write(w, http.StatusOK, handler.issueResponse(issue))
}

// PATCH /api/v1/issues/{id} {"status": "resolved" | "unresolved" | "ignored"}
func (handler *Handler) updateIssue(w http.ResponseWriter, r *http.Request) {
	issueID, err := pathID(r, "issueID")

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	var body struct {
		Status string `json:"status"`
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&body); err != nil {
		handler.fail(w, r, invalid("invalid JSON body: %v", err))
		return
	}

	switch body.Status {
	case store.StatusUnresolved, store.StatusResolved, store.StatusIgnored:
	default:
		handler.fail(w, r, invalid("status must be unresolved, resolved or ignored"))
		return
	}

	if err := handler.store.SetIssueStatus(r.Context(), issueID, body.Status, apiActor); err != nil {
		handler.fail(w, r, err)
		return
	}

	handler.getIssue(w, r)
}

// apiActor is who changes a status through the REST API: its one token.
var apiActor = store.Actor{Name: "API token", Source: store.SourceAPI}

func (handler *Handler) deleteIssue(w http.ResponseWriter, r *http.Request) {
	issueID, err := pathID(r, "issueID")

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	if err := handler.store.DeleteIssue(r.Context(), issueID); err != nil {
		handler.fail(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/issues/{id}/events?limit=&cursor= — newest first, without payloads.
func (handler *Handler) listIssueEvents(w http.ResponseWriter, r *http.Request) {
	issueID, err := pathID(r, "issueID")

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	if _, err := handler.store.GetIssue(r.Context(), issueID); err != nil {
		handler.fail(w, r, err)
		return
	}

	limit, err := parseLimit(r)

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	beforeID, err := decodeCursor(r.URL.Query().Get("cursor"), 'i')

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	events, err := handler.store.ListEvents(r.Context(), store.EventFilter{IssueID: issueID, BeforeID: beforeID, Limit: limit + 1})

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	items, more := store.Page(events, limit)
	result := page[store.EventSummary]{Items: items}

	if more {
		result.NextCursor = encodeCursor('i', items[limit-1].ID)
	}

	httpjson.Write(w, http.StatusOK, result)
}

func (handler *Handler) latestEvent(w http.ResponseWriter, r *http.Request) {
	issueID, err := pathID(r, "issueID")

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	event, err := handler.store.LatestEvent(r.Context(), issueID)

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	httpjson.Write(w, http.StatusOK, event)
}

func (handler *Handler) getEvent(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "projectID")

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	event, err := handler.store.GetEvent(r.Context(), int(projectID), sentry.NormalizeEventID(r.PathValue("eventID")))

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	httpjson.Write(w, http.StatusOK, event)
}

// GET /api/v1/logs?project=&level=&query=&trace_id=&since=&until=&limit=&cursor=
func (handler *Handler) listLogs(w http.ResponseWriter, r *http.Request) {
	filter, err := handler.logFilter(r)

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	limit := filter.Limit
	filter.Limit++
	logs, err := handler.store.ListLogs(r.Context(), filter)

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	items, more := store.Page(logs, limit)
	result := page[store.LogRecord]{Items: items}

	if more {
		result.NextCursor = encodeCursor('i', items[limit-1].ID)
	}

	httpjson.Write(w, http.StatusOK, result)
}

func (handler *Handler) logFilter(r *http.Request) (store.LogFilter, error) {
	query := r.URL.Query()
	filter := store.LogFilter{Levels: levelsFromQuery(query), Query: query.Get("query"), TraceID: query.Get("trace_id")}

	var err error

	if filter.ProjectIDs, err = handler.projectsFromQuery(r); err != nil {
		return filter, err
	}

	if filter.Since, err = parseTimeParam(r, "since"); err != nil {
		return filter, err
	}

	if filter.Until, err = parseTimeParam(r, "until"); err != nil {
		return filter, err
	}

	if filter.Limit, err = parseLimit(r); err != nil {
		return filter, err
	}

	filter.BeforeID, err = decodeCursor(query.Get("cursor"), 'i')

	return filter, err
}

// GET /api/v1/traces/{traceID} — events and logs of one trace (e.g. one
// request), so an error can be seen together with the logs around it.
func (handler *Handler) getTrace(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceID")
	events, err := handler.store.ListEvents(r.Context(), store.EventFilter{TraceID: traceID, Limit: maxLimit})

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	logs, err := handler.store.ListLogs(r.Context(), store.LogFilter{TraceID: traceID, Limit: maxLimit})

	if err != nil {
		handler.fail(w, r, err)
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{"trace_id": traceID, "events": events, "logs": logs})
}
