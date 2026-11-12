package ui

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/store"
)

// A list URL can be edited by hand. Before anything else, a list keeps only
// the parameters it understands, with valid values; the query to the
// database, the filter fields, the chips and every link are then built from
// that one cleaned query, so the page never shows a filter that is not
// applied, nor applies one it does not show.

// maxSearchLength caps the search text: a LIKE over a megabyte of pattern
// helps nobody.
const maxSearchLength = 200

var issueStatuses = []string{store.StatusUnresolved, store.StatusResolved, store.StatusIgnored, "all"}

type listParams struct {
	levels   []string // the levels of this list
	single   []string // free-text parameters, one value each
	statuses bool     // ?status= (issues)
	sort     bool     // ?sort= (issues)
}

var (
	issueListParams = listParams{levels: eventLevels, single: []string{"query"}, statuses: true, sort: true}
	logListParams   = listParams{levels: logLevels, single: []string{"query", "trace_id"}}
)

func (handler *Handler) cleanListQuery(raw url.Values, params listParams) url.Values {
	query := url.Values{}

	// Several values: known ones only, each once. Projects are kept by name,
	// also when given by id, so chips and colors find them.
	for _, value := range raw["project"] {
		if project, ok := handler.registry.List().Find(strings.TrimSpace(value)); ok && !slices.Contains(query["project"], project.Name) {
			query.Add("project", project.Name)
		}
	}

	for _, value := range raw["level"] {
		if slices.Contains(params.levels, value) && !slices.Contains(query["level"], value) {
			query.Add("level", value)
		}
	}

	for _, key := range params.single {
		if value := strings.TrimSpace(raw.Get(key)); value != "" {
			query.Set(key, truncateRunes(value, maxSearchLength))
		}
	}

	if status := raw.Get("status"); params.statuses && slices.Contains(issueStatuses, status) {
		query.Set("status", status)
	}

	if sort := raw.Get("sort"); params.sort && sort != "" && store.ValidIssueSort(sort) {
		query.Set("sort", sort)
	}

	for _, key := range []string{"from", "to"} {
		if _, err := time.Parse(time.DateOnly, raw.Get(key)); err == nil {
			query.Set(key, raw.Get(key))
		}
	}

	if page, err := strconv.Atoi(raw.Get("page")); err == nil && page > 1 {
		query.Set("page", strconv.Itoa(page))
	}

	if perPage, err := strconv.Atoi(raw.Get("per_page")); err == nil && slices.Contains(perPageChoices, perPage) {
		query.Set("per_page", strconv.Itoa(perPage))
	}

	return query
}

func truncateRunes(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit])
	}

	return text
}
