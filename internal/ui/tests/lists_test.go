package tests

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/fixtures"
)

// letters names an issue by its index without digits (messages are grouped
// with numbers replaced, so "error 1" and "error 2" would be one issue).
func letters(index int) string {
	return string(rune('a'+index/26)) + string(rune('a'+index%26))
}

func TestPagination(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	for index := range 45 {
		envelope := `{}` + "\n" + `{"type":"event"}` + "\n" + fmt.Sprintf(`{"message":"paged issue %s","level":"error"}`, letters(index))
		server.sendEnvelope(fixtures.BackendEnvelopeURL, []byte(envelope))
	}

	first := server.page("/issues")

	if count := len(issueIDs(first)); count != 20 || !strings.Contains(first, "1–20 of 45") {
		t.Fatalf("first page: %d issues", count)
	}

	for _, want := range []string{`href="/issues?page=2"`, `href="/issues?page=3"`, `<span class="current" aria-current="page">1</span>`, `<option value="20" selected>20 per page</option>`} {
		if !strings.Contains(first, want) {
			t.Errorf("first page is missing %s", want)
		}
	}

	if last := server.page("/issues?page=3"); len(issueIDs(last)) != 5 || !strings.Contains(last, "41–45 of 45") || !strings.Contains(last, `rel="prev"`) {
		t.Errorf("last page: %d issues", len(issueIDs(last)))
	}

	// A page past the end shows the last one.
	if beyond := server.page("/issues?page=99"); !strings.Contains(beyond, "41–45 of 45") {
		t.Error("a page past the end must show the last page")
	}

	// A bigger page size keeps the filters; with one page there are no links.
	all := server.page("/issues?per_page=50&level=error")

	// The page size sits with the pages under the list and belongs to the
	// filter form, so it is sent with the filters.
	pager := all[strings.Index(all, `<nav class="pager"`):]

	if !strings.Contains(pager, `<select name="per_page" aria-label="Rows per page" form="issue-filters">`) || !strings.Contains(pager, `<option value="50" selected>`) {
		t.Error("the page size is not under the issue list")
	}

	if count := len(issueIDs(all)); count != 45 || strings.Contains(all, `rel="next"`) || !strings.Contains(all, `<option value="50" selected>`) {
		t.Errorf("per_page=50: %d issues", count)
	}

	// Filter links start again from the first page but keep the page size.
	if page := server.page("/issues?per_page=50&page=2"); !strings.Contains(page, `href="/issues?level=error&amp;per_page=50"`) {
		t.Error("a level link must drop the page and keep per_page")
	}

	// An unknown size falls back to the default.
	if odd := server.page("/issues?per_page=7"); len(issueIDs(odd)) != 20 {
		t.Errorf("per_page=7: %d issues", len(issueIDs(odd)))
	}
}

func TestLogPagination(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	var items []string

	// Different words, so the cap on a repeated message keeps them all.
	for index := range 25 {
		items = append(items, fmt.Sprintf(`{"timestamp":1731400000.%03d,"level":"info","body":"paged log %d %s"}`, index, index, strings.Repeat("x", index+1)))
	}

	server.sendEnvelope(fixtures.BackendEnvelopeURL, []byte(`{}`+"\n"+`{"type":"log","item_count":25,"content_type":"application/vnd.sentry.items.log+json"}`+"\n"+`{"items":[`+strings.Join(items, ",")+`]}`))

	first := server.page("/logs")

	if !strings.Contains(first, `<form class="filters" id="log-filters"`) || !strings.Contains(first, `form="log-filters">`) {
		t.Error("the log page size does not belong to the log filters")
	}

	if !strings.Contains(first, "1–20 of 25") || !strings.Contains(first, "paged log 24 ") || strings.Contains(first, "paged log 4 ") {
		t.Fatal("first page of logs")
	}

	if second := server.page("/logs?page=2"); !strings.Contains(second, "21–25 of 25") || !strings.Contains(second, "paged log 0 ") {
		t.Fatal("second page of logs")
	}
}

// A hand-edited URL: unknown or invalid values are dropped before anything
// else, so they neither filter nor show up as chips or in links.
func TestHandEditedListURL(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	plain := server.page("/issues")
	odd := server.page("/issues?status=hack&sort=drop%20table&from=garbage&to=2026-13-45&level=nope&project=ghost&page=-3&per_page=7&evil=%3Cscript%3E")

	if len(issueIDs(odd)) != len(issueIDs(plain)) || len(issueIDs(plain)) == 0 {
		t.Fatalf("invalid values must be ignored: %d issues, %d without them", len(issueIDs(odd)), len(issueIDs(plain)))
	}

	for _, unwanted := range []string{`class="active-filters"`, "hack", "drop table", "garbage", "nope", "ghost", "evil", "<script>"} {
		if strings.Contains(odd, unwanted) {
			t.Errorf("%q must not appear on the page", unwanted)
		}
	}

	// A project given by id is shown by name, with its color.
	if byID := server.page("/issues?project=1"); !strings.Contains(byID, `Project: <span class="project" data-project="backend">backend`) {
		t.Error("a project given by id must show as its name")
	}

	// Repeated values count once.
	if repeated := server.page("/issues?level=error&level=error"); strings.Count(repeated, `<span class="badge level-error">error<a`) != 1 {
		t.Error("a repeated level must give one chip")
	}

	// The search is capped.
	long := strings.Repeat("x", 5000)

	if page := server.page("/issues?query=" + long); strings.Contains(page, strings.Repeat("x", 201)) {
		t.Error("the search text must be capped")
	}

	if page := server.page("/logs?level=nope&trace_id=%20&page=abc"); strings.Contains(page, `class="active-filters"`) {
		t.Error("logs: invalid values must give no chips")
	}
}

func TestHTMXFragments(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	request := httptest.NewRequest(http.MethodGet, "/issues?project=frontend", nil)
	request.Header.Set("HX-Request", "true")
	request.Header.Set("HX-Target", "issue-list")

	body := server.send(request).Body.String()

	if strings.Contains(body, "<html") || !strings.Contains(body, `id="issue-list"`) {
		t.Fatalf("filter request must return only the list:\n%s", body)
	}

	if len(issueIDs(body)) != 4 {
		t.Fatalf("frontend filter: %d issues, want 4", len(issueIDs(body)))
	}

	logs := httptest.NewRequest(http.MethodGet, "/logs?query=pino", nil)
	logs.Header.Set("HX-Request", "true")
	logs.Header.Set("HX-Target", "log-list")

	body = server.send(logs).Body.String()

	if strings.Contains(body, "<html") || !strings.Contains(body, "pino") {
		t.Fatalf("log search fragment:\n%s", body)
	}
}

func TestListFilters(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	// A level badge links to the same list narrowed by that level, keeping
	// the other filters and dropping pagination.
	list := server.page("/issues?project=backend&offset=0")

	if !strings.Contains(list, `href="/issues?level=warning&amp;project=backend"`) {
		t.Fatalf("level badge must link to a filtered list:\n%s", list)
	}

	warnings := server.page("/issues?level=warning&status=all")

	if ids := issueIDs(warnings); len(ids) != 1 {
		t.Fatalf("level filter: %d issues, want 1", len(ids))
	}

	today := time.Now().UTC().Format(time.DateOnly)
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)

	if ids := issueIDs(server.page("/issues?status=all&from=" + today + "&to=" + today)); len(ids) != 9 {
		t.Fatalf("today: %d issues, want 9", len(ids))
	}

	if ids := issueIDs(server.page("/issues?status=all&from=" + tomorrow)); len(ids) != 0 {
		t.Fatalf("from tomorrow: %d issues, want 0", len(ids))
	}

	// Log times come from the SDK: the fixtures were recorded on 2026-11-10.
	logs := server.page("/logs?from=2026-11-10")

	if !strings.Contains(logs, "full-screen") || !strings.Contains(logs, `href="/logs?from=2026-11-10&amp;project=backend"`) {
		t.Fatalf("logs page: missing full-screen button or project filter link:\n%s", logs)
	}

	if strings.Contains(server.page("/logs?to=2000-01-01"), `class="log-text"`) {
		t.Fatal("logs before 2000 must be empty")
	}
}

// Several projects can be chosen: the list has the issues of each, and each
// project has its own chip that removes only it.
func TestSeveralProjectsFilter(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	both := server.page("/issues?project=backend&project=frontend")
	backendOnly := server.page("/issues?project=backend")
	frontendOnly := server.page("/issues?project=frontend")

	if count := len(issueIDs(both)); count == 0 || count != len(issueIDs(backendOnly))+len(issueIDs(frontendOnly)) {
		t.Fatalf("both projects: %d issues, backend %d, frontend %d", count, len(issueIDs(backendOnly)), len(issueIDs(frontendOnly)))
	}

	for _, want := range []string{
		// One chip for all chosen projects; each × removes only its project.
		`Project: <span class="project" data-project="backend">backend<a class="badge-remove" href="/issues?project=frontend"`,
		`<span class="project" data-project="frontend">frontend<a class="badge-remove" href="/issues?project=backend"`,
		`name="project" value="backend" checked`, `name="project" value="frontend" checked`,
	} {
		if !strings.Contains(both, want) {
			t.Errorf("missing %s", want)
		}
	}

	if chips := strings.Count(both, `<span class="chip">Project:`); chips != 1 {
		t.Errorf("%d project chips, want one for all chosen projects", chips)
	}

	// "All" is checked only while nothing is chosen.
	// With projects chosen only "All levels" is checked.
	if strings.Count(both, `class="picker-clear" checked`) != 1 {
		t.Error("All projects must not be checked with projects chosen")
	}

	if plain := server.page("/issues"); strings.Count(plain, `class="picker-clear" checked`) != 2 {
		t.Error("All projects and All levels must be checked without filters")
	}

	// The field says how many are chosen; the values are in the chip.
	if !strings.Contains(both, `data-name="Projects">Projects: 2</summary>`) || !strings.Contains(both, `data-name="Levels">All levels</summary>`) {
		t.Error("picker summaries")
	}

	// The log list filters by several levels too.
	logs := server.page("/logs?level=warn&level=info")

	if !strings.Contains(logs, `<a class="badge level-warning"`) || !strings.Contains(logs, `<a class="badge level-info"`) ||
		strings.Contains(logs, `<a class="badge level-error"`) || strings.Contains(logs, `<a class="badge level-debug"`) {
		t.Error("logs filtered by warn and info show other levels or miss these")
	}

	// Levels work the same way.
	levels := server.page("/issues?level=error&level=info&status=all")
	errorOnly := server.page("/issues?level=error&status=all")
	infoOnly := server.page("/issues?level=info&status=all")

	if count := len(issueIDs(levels)); count == 0 || count != len(issueIDs(errorOnly))+len(issueIDs(infoOnly)) || len(issueIDs(infoOnly)) == 0 {
		t.Errorf("error and info: %d issues, error %d, info %d", count, len(issueIDs(errorOnly)), len(issueIDs(infoOnly)))
	}

	for _, want := range []string{
		`Level: <span class="badge level-error">error<a class="badge-remove" href="/issues?level=info&amp;status=all"`,
		`name="level" value="error" checked`, `name="level" value="info" checked`,
	} {
		if !strings.Contains(levels, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestActiveFilterChips(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	body := server.page("/issues?project=backend&level=error&status=unresolved&sort=event_count&per_page=50&page=2")

	// Applied filters, each with a link that removes only it (and restarts pagination).
	for _, want := range []string{
		`Project: <span class="project" data-project="backend">backend<a class="badge-remove" href="/issues?level=error&amp;per_page=50&amp;sort=event_count&amp;status=unresolved"`,
		`Level: <span class="badge level-error">error<a class="badge-remove" href="/issues?per_page=50&amp;project=backend&amp;sort=event_count&amp;status=unresolved"`,
		// The sort order is shown and removed like a filter.
		`Sort: <strong>most events</strong><a class="chip-remove" href="/issues?level=error&amp;per_page=50&amp;project=backend&amp;status=unresolved"`,
		// Clear all keeps only the page size: it is not a filter.
		`<a class="clear-filters" href="/issues?per_page=50">Clear all</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}

	// Defaults are not filters.
	for _, unwanted := range []string{"Status: <strong>", "Dates: <strong>"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("chip %q must not be shown", unwanted)
		}
	}

	// Both ends of the range are one chip, removed together.
	dates := server.page("/issues?from=2026-11-01&to=2026-11-10&level=error")

	if !strings.Contains(dates, `Dates: <strong>2026-11-01 – 2026-11-10</strong><a class="chip-remove" href="/issues?level=error"`) || strings.Count(dates, `<span class="chip">`) != 2 {
		t.Errorf("dates chip:\n%s", dates)
	}

	if strings.Contains(server.page("/issues"), `class="active-filters"`) {
		t.Error("no chips without filters")
	}

	logs := server.page("/logs?level=warn&trace_id=abc")

	if !strings.Contains(logs, "Trace: <strong>abc</strong>") || !strings.Contains(logs, `<a class="clear-filters" href="/logs">Clear all</a>`) {
		t.Errorf("logs chips:\n%s", logs)
	}
}

// A reversed range ("to" before "from") would match nothing; it is swapped.
func TestReversedDateRangeIsSwapped(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	today := time.Now().UTC().Format(time.DateOnly)
	weekAgo := time.Now().UTC().AddDate(0, 0, -7).Format(time.DateOnly)

	reversed := server.page("/issues?status=all&from=" + today + "&to=" + weekAgo)

	if ids := issueIDs(reversed); len(ids) != 9 {
		t.Fatalf("reversed range: %d issues, want 9 (as for the swapped range)", len(ids))
	}

	// The inputs show the corrected range and keep each other in bounds.
	for _, want := range []string{
		`name="from" value="` + weekAgo + `" max="` + today + `"`,
		// Neither picker offers days after today.
		`name="to" value="` + today + `" min="` + weekAgo + `" max="` + today + `"`,
	} {
		if !strings.Contains(reversed, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// The pickers never offer a day after today: there are no events from the future.
func TestDatePickersStopAtToday(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	today := time.Now().UTC().Format(time.DateOnly)
	body := server.page("/issues")

	for _, want := range []string{`name="from" value="" max="` + today + `"`, `name="to" value="" min="" max="` + today + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// Project and level badges are links: within the issue and log lists they
// filter that list, everywhere else they open the issues with that filter.
// Inside checkboxes they are plain labels.
func TestBadgeLinks(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	issueID := issueIDs(server.page("/issues?project=backend&level=error"))[0]
	issue := server.page("/issues/" + issueID)

	for _, want := range []string{
		`<a class="project" data-project="backend" href="/issues?project=backend"`,
		`<a class="badge level-error" href="/issues?level=error"`,
		`<a class="status status-unresolved" href="/issues?status=unresolved"`,
	} {
		if !strings.Contains(issue, want) {
			t.Errorf("issue page is missing %s", want)
		}
	}

	if list := server.page("/issues?status=all"); !strings.Contains(list, `<a class="project" data-project="backend" href="/issues?project=backend&amp;status=all"`) {
		t.Error("in the issue list a project badge filters the list and keeps its filters")
	}

	// A status filter is a chip with the status badge; the default is not.
	if resolved := server.page("/issues?status=resolved"); !strings.Contains(resolved, `Status: <span class="status status-resolved">`) {
		t.Error("status chip")
	}

	if strings.Contains(server.page("/issues?status=unresolved"), `class="active-filters"`) {
		t.Error("the default status is not a filter")
	}

	logs := server.page("/logs?query=a")

	if !strings.Contains(logs, `<a class="badge level-warning" href="/logs?level=warn&amp;query=a"`) && !strings.Contains(logs, `href="/logs?level=info&amp;query=a"`) {
		t.Error("in the log list a level badge filters the log list")
	}

	// Labels inside checkboxes are not links.
	if strings.Contains(logs, `<input type="checkbox" name="project" value="backend" > <a`) || !strings.Contains(logs, `value="backend" > <span class="project"`) {
		t.Error("badges in checkboxes must be labels")
	}

	if page := server.page("/projects"); !strings.Contains(page, `<a class="project" data-project="frontend" href="/issues?project=frontend"`) {
		t.Error("projects table")
	}
}

func TestLogsPage(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	logs := server.page("/logs")

	if strings.Contains(logs, "\x1b[") {
		t.Fatal("ANSI color codes must be removed from log lines")
	}

	if !strings.Contains(server.page("/logs?level=warn"), "slow query") {
		t.Fatal("level filter")
	}
}

// The issue list is sorted by its column headings, one sort at a time; the
// chosen sort belongs to the filter form, so changing a filter keeps it.
func TestIssueSortHeadings(t *testing.T) {
	server := newTestServer(t, password)
	server.login()
	server.ingestFixtures()

	page := html.UnescapeString(server.page("/issues"))

	for _, want := range []string{
		`<th class="sortable number"><a href="/issues?sort=event_count"`,
		`<th class="sortable" aria-sort="descending"><a href="/issues?sort=last_seen_asc"`,
		`<th class="sortable"><a href="/issues?sort=project"`,
		`<th class="sortable number"><a href="/issues?sort=id"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %s", want)
		}
	}

	if strings.Contains(page, `<select name="sort"`) || strings.Contains(page, `name="sort" value=`) {
		t.Error("the default sort needs no field")
	}

	sorted := html.UnescapeString(server.page("/issues?sort=event_count&page=1"))

	if !strings.Contains(sorted, `<input type="hidden" name="sort" value="event_count" form="issue-filters">`) ||
		!strings.Contains(sorted, `aria-sort="descending"><a href="/issues?sort=event_count_asc"`) {
		t.Errorf("sorted by events:\n%s", sorted)
	}
}
