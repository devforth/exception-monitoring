package controllers

import (
	"net/http"
	"strings"

	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
	"github.com/devforth/exception-monitoring/internal/ui/presenters"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// LogsPage searches logs, newest first. Like IssuesPage, htmx gets only
// "log-list" for filters and page links.
func (controller *Controllers) LogsPage(w http.ResponseWriter, r *http.Request) {
	query := lists.CleanListQuery(r.URL.Query(), lists.LogListParams, controller.registry)
	projectIDs := controller.projects.IDs(query["project"])
	data := contracts.LogsData{
		Projects: lists.NewPicker("project", "project", "All projects", "Projects", controller.projects.Names(), query["project"]),
		Query:    query.Get("query"),
		Dates:    lists.ParseDateRange(query),
		TraceID:  strings.TrimSpace(query.Get("trace_id")),
		Levels:   lists.NewPicker("level", "level", "All levels", "Levels", lists.LogLevels, query["level"]),
		Filter:   lists.NewFilterLinks(r, query),
	}

	page, perPage := lists.ReadPaging(query)
	filter := store.LogFilter{
		ProjectIDs: projectIDs,
		Levels:     query["level"],
		Query:      data.Query,
		TraceID:    data.TraceID,
		Since:      data.Dates.Since,
		Until:      data.Dates.Until,
	}

	total, err := controller.logs.Count(r.Context(), filter)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	data.Pager = lists.NewPager(data.Filter, page, perPage, total, "log-list")
	filter.Limit, filter.Offset = perPage, (data.Pager.Page-1)*perPage

	logs, err := controller.logs.List(r.Context(), filter)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	issues, err := controller.logs.Issues(r.Context(), logs)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	data.Logs = presenters.LogRows(logs, issues, controller.projects.Name)

	switch {
	case requests.IsHTMX(r) && r.Header.Get("HX-Target") == "log-list":
		controller.views.RenderFragment(w, r, "logs", "log-list", data)
	default:
		controller.views.Render(w, r, http.StatusOK, "logs", data)
	}
}
