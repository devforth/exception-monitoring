package lists

import (
	"net/url"
	"testing"
)

func TestActiveFilterChips(t *testing.T) {
	links := testLinks(url.Values{
		"project": {"backend", "frontend"}, "status": {"unresolved"}, "from": {"2026-11-01"}, "to": {"2026-11-10"},
		"sort": {"event_count"}, "per_page": {"50"}, "page": {"2"},
	})
	filters := links.IssueFilters()

	if len(filters.Items) != 3 {
		t.Fatalf("chips = %+v, want project, dates and sort (the default status is not a filter)", filters.Items)
	}

	projects := filters.Items[0]

	if projects.Label != "Project" || len(projects.Badges) != 2 {
		t.Fatalf("project chip = %+v, want one chip with two badges", projects)
	}

	// Removing one project keeps the other and every other filter, from page 1.
	want := "/issues?from=2026-11-01&per_page=50&project=frontend&sort=event_count&status=unresolved&to=2026-11-10"

	if got := projects.Badges[0].RemoveURL; got != want {
		t.Errorf("remove backend = %q, want %q", got, want)
	}

	dates := filters.Items[1]

	if dates.Value != "2026-11-01 – 2026-11-10" || dates.RemoveURL != "/issues?per_page=50&project=backend&project=frontend&sort=event_count&status=unresolved" {
		t.Errorf("dates chip = %+v, want one chip removing both ends", dates)
	}

	if sort := filters.Items[2]; sort.Label != "Sort" || sort.Value != "most events" {
		t.Errorf("sort chip = %+v, want the sort in words", sort)
	}

	if filters.ClearURL != "/issues?per_page=50" {
		t.Errorf("clear all = %q, want only the page size kept", filters.ClearURL)
	}

	if chips := testLinks(url.Values{"sort": {"last_seen"}, "from": {"2026-11-01"}}).IssueFilters().Items; len(chips) != 1 || chips[0].Value != "from 2026-11-01" {
		t.Errorf("chips = %+v, want only the open date range (default sort is no filter)", chips)
	}
}

// One sort at a time: a column sorts in its natural direction first, the
// next click reverses it; the page starts over and the default is no ?sort=.
func TestSortHeader(t *testing.T) {
	for _, test := range []struct {
		sort, field, direction, next string
	}{
		{"", "last_seen", "desc", "last_seen_asc"},
		{"last_seen_asc", "last_seen", "asc", ""},
		{"", "event_count", "", "event_count"},
		{"event_count", "event_count", "desc", "event_count_asc"},
		{"event_count", "last_seen", "", ""},
		{"", "project", "", "project"},
		{"project", "project", "asc", "project_desc"},
		{"project_desc", "project", "desc", "project"},
		{"", "id", "", "id"},
		{"id", "id", "desc", "id_asc"},
	} {
		query := url.Values{"page": {"3"}, "level": {"error"}}

		if test.sort != "" {
			query.Set("sort", test.sort)
		}

		header := testLinks(query).SortHeader(test.field, "Label", "")
		link, _ := url.Parse(header.URL)

		if header.Direction != test.direction || link.Query().Get("sort") != test.next || link.Query().Has("page") || link.Query().Get("level") != "error" {
			t.Errorf("sort %q, column %s: direction %q, link %s", test.sort, test.field, header.Direction, header.URL)
		}
	}
}
