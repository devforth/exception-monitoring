package lists

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/text"
	"github.com/devforth/exception-monitoring/internal/storage/store"
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

// The level lists of the filters, highest first.
var (
	EventLevels = config.HighestFirst(config.Levels)
	LogLevels   = config.HighestFirst(config.LogLevels)
)

// ListParams are the parameters a list understands.
type ListParams struct {
	levels   []string // the levels of this list
	single   []string // free-text parameters, one value each
	statuses bool     // ?status= (issues)
	sort     bool     // ?sort= (issues)
}

var (
	IssueListParams = ListParams{levels: EventLevels, single: []string{"query"}, statuses: true, sort: true}
	LogListParams   = ListParams{levels: LogLevels, single: []string{"query", "trace_id"}}
)

// CleanListQuery keeps the parameters of raw that params understands, with
// valid values; projects are looked up in projectList.
func CleanListQuery(raw url.Values, params ListParams, projectList config.ProjectList) url.Values {
	query := url.Values{}

	// Several values: known ones only, each once. Projects are kept by name,
	// also when given by id, so chips and colors find them.
	for _, value := range raw["project"] {
		if project, ok := projectList.List().Find(strings.TrimSpace(value)); ok && !slices.Contains(query["project"], project.Name) {
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
			query.Set(key, text.TruncateRunes(value, maxSearchLength))
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

	if perPage, err := strconv.Atoi(raw.Get("per_page")); err == nil && slices.Contains(PerPageChoices, perPage) {
		query.Set("per_page", strconv.Itoa(perPage))
	}

	return query
}
