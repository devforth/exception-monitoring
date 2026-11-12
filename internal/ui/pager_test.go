package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

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

	for index := range 25 {
		items = append(items, fmt.Sprintf(`{"timestamp":1731400000.%03d,"level":"info","body":"paged log %d"}`, index, index))
	}

	server.sendEnvelope(fixtures.BackendEnvelopeURL, []byte(`{}`+"\n"+`{"type":"log","item_count":25,"content_type":"application/vnd.sentry.items.log+json"}`+"\n"+`{"items":[`+strings.Join(items, ",")+`]}`))

	first := server.page("/logs")

	if !strings.Contains(first, "1–20 of 25") || !strings.Contains(first, "paged log 24") || strings.Contains(first, "paged log 4<") {
		t.Fatal("first page of logs")
	}

	if second := server.page("/logs?page=2"); !strings.Contains(second, "21–25 of 25") || !strings.Contains(second, "paged log 0") {
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

func TestBulkActionLimit(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	form := url.Values{"action": {"resolve"}, "return": {"/issues"}}

	for index := range 1001 {
		form.Add("issue", fmt.Sprint(index+1))
	}

	if response := server.post("/issues/bulk", form); response.Code != http.StatusBadRequest {
		t.Fatalf("1001 issues at once: %d", response.Code)
	}
}
