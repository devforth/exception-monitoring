package tests

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/fixtures"
)

func TestEveryIssuePageRenders(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	ids := issueIDs(server.page("/issues?status=all"))

	if len(ids) != 9 {
		t.Fatalf("found %d issues, want 9", len(ids))
	}

	for _, id := range ids {
		body := server.page("/issues/" + id)

		if !strings.Contains(body, "Raw event JSON") {
			t.Fatalf("issue %s: event details missing", id)
		}
	}
}

func TestIssueDetails(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	list := server.page("/issues?query=Failed+to+load+orders")
	ids := issueIDs(list)

	if len(ids) != 1 {
		t.Fatalf("search found %d issues", len(ids))
	}

	body := server.page("/issues/" + ids[0])

	prompt := regexp.MustCompile(`<textarea id="agent-fix-prompt" hidden>([\s\S]*?)</textarea>`).FindStringSubmatch(body)

	if len(prompt) != 2 {
		t.Fatal("missing hidden agent prompt")
	}

	for _, want := range []string{"connect ECONNREFUSED", "## Exception", "## Breadcrumbs", "Logs of the same", "Events: ", "Do not change any code"} {
		if !strings.Contains(html.UnescapeString(prompt[1]), want) {
			t.Errorf("agent prompt is missing %q", want)
		}
	}

	if !strings.Contains(body, `data-copy="agent-fix-prompt"`) {
		t.Fatal("copy prompt button must not submit the issue action form")
	}

	// The MCP request of notifications, on the list and the page.
	reference := `data-copy-text="Exception Monitoring MCP: get_issue {"issue_id":"` + ids[0] + `"}"`

	for name, page := range map[string]string{"list": list, "issue page": body} {
		if !strings.Contains(html.UnescapeString(page), reference) {
			t.Errorf("%s lacks the MCP request copy button", name)
		}
	}

	// Columns: checkbox, then the issue (the ID is on the issue page only);
	// the row shows the status (its meaning on hover).
	row := `<tr class="issue-unresolved" title="Unresolved: still happening or not looked at yet">
  <td class="check"><input type="checkbox" name="issue" value="` + ids[0] + `" aria-label="Select issue ` + ids[0] + `"></td>
  <td>
    `

	if !strings.Contains(list, row) || strings.Contains(list, "<th>ID</th>") {
		t.Error("the issue list is not checkbox, then the issue")
	}

	if !strings.Contains(body, "<dt>ID</dt><dd>"+ids[0]+"</dd>") {
		t.Error("the issue page does not show the issue ID")
	}

	for _, want := range []string{
		"Caused by",            // cause chain
		"connect ECONNREFUSED", // inner error
		`class="line current"`, // source line of the throwing frame
		"Breadcrumbs",
		"Logs from the same request", // pino log of the same request, linked by trace_id
	} {
		if !strings.Contains(body, want) {
			t.Errorf("issue page is missing %q", want)
		}
	}

	if !strings.Contains(body, `<a href="#events" title="Show the events">`) || !strings.Contains(body, `<section id="events">`) {
		t.Error("the event count does not lead to the events")
	}

	// The stack and the event's details start closed, their headings the toggles.
	if !strings.Contains(body, `<details class="section stacktrace">`) {
		t.Error("the stack trace is not a closed section")
	}

	for _, section := range []string{"Logs from the same request", "Breadcrumbs", "Request"} {
		if !strings.Contains(body, `<details class="section">
  <summary><h2>`+section+`</h2>`) {
			t.Errorf("%s is not a closed section", section)
		}
	}
}

func TestEventTextIsEscaped(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	envelope := `{}` + "\n" + `{"type":"event"}` + "\n" +
		`{"exception":{"values":[{"type":"Error","value":"<script>alert(1)</script>"}]},"tags":{"<b>":"<img src=x onerror=alert(1)>"}}`
	server.sendEnvelope(fixtures.BackendEnvelopeURL, []byte(envelope))

	list := server.page("/issues")
	id := issueIDs(list)[0]

	for _, body := range []string{list, server.page("/issues/" + id)} {
		if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
			t.Fatal("event text must be HTML-escaped")
		}
	}
}

// A frame unfolds only when it has something to show: browser SDKs send no
// source, so their frames are plain rows.
func TestOnlyFramesWithSourceUnfold(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	envelope := `{}` + "\n" + `{"type":"event"}` + "\n" +
		`{"exception":{"values":[{"type":"Error","value":"frames","stacktrace":{"frames":[
			{"filename":"/app/bare.js","function":"bare","lineno":1,"in_app":true},
			{"filename":"/app/source.js","function":"withSource","lineno":2,"in_app":true,"context_line":"throw error"}]}}]}}`
	server.sendEnvelope(fixtures.BackendEnvelopeURL, []byte(strings.ReplaceAll(envelope, "\n\t\t\t", "")))

	body := server.page("/issues/" + issueIDs(server.page("/issues"))[0])

	if !regexp.MustCompile(`<details class="frame in-app" open>\s*<summary><span class="function">withSource`).MatchString(body) {
		t.Error("the frame with source does not unfold")
	}

	if !strings.Contains(body, `<div class="frame frame-bare in-app"><span class="function">bare</span>`) {
		t.Error("the frame without source is not a plain row")
	}
}

// When an issue was resolved is shown on its page and in the list of
// resolved issues.
func TestResolvedAt(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	id := issueIDs(server.page("/issues"))[0]

	if response := server.post("/issues/"+id+"/action", url.Values{"action": {"resolve"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("resolve: %d", response.Code)
	}

	if page := server.page("/issues/" + id); !strings.Contains(page, "<dt>Resolved</dt><dd><time") {
		t.Error("the issue page must show when it was resolved")
	}

	if list := server.page("/issues?status=resolved"); !strings.Contains(list, "<th>Resolved</th>") || strings.Count(list, "<td><time") < 3 {
		t.Error("the resolved list must have a Resolved column with the time")
	}

	if list := server.page("/issues"); strings.Contains(list, "<th>Resolved</th>") {
		t.Error("unresolved issues have no Resolved column")
	}

	// Reopened, it is not resolved any more.
	server.post("/issues/"+id+"/action", url.Values{"action": {"reopen"}})

	if page := server.page("/issues/" + id); strings.Contains(page, "<dt>Resolved</dt>") {
		t.Error("a reopened issue must not show a resolve time")
	}
}

// The trace ID of an event links to the logs of the trace only when it has
// some; the event's details come before the list of events.
func TestTraceLink(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	const withLogs, withoutLogs = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	logged, logs := requestEnvelopes(withLogs, "1111111111111111", "2222222222222222", "logged failure", "a log line")
	alone, _ := requestEnvelopes(withoutLogs, "3333333333333333", "", "alone failure", "")

	for _, envelope := range [][]byte{logs, logged, alone} {
		server.sendEnvelope(fixtures.BackendEnvelopeURL, envelope)
	}

	pages := map[string]string{}

	for _, id := range issueIDs(server.page("/issues")) {
		page := server.page("/issues/" + id)
		pages[strings.Split(strings.SplitN(page, "<title>", 2)[1], " ")[1]] = page
	}

	if !strings.Contains(pages["logged"], `href="/logs?trace_id=`+withLogs+`"`) {
		t.Error("a trace with logs links to them")
	}

	if strings.Contains(pages["alone"], `href="/logs?trace_id=`+withoutLogs+`"`) {
		t.Error("a trace without logs must not link to an empty log list")
	}

	// The events follow the exception, before the logs of the request.
	if page, position := pages["logged"], strings.Index; position(page, "<h2>Exception</h2>") > position(page, "<h2>Events</h2>") ||
		position(page, "<h2>Events</h2>") > position(page, "<h2>Logs from the same request</h2>") {
		t.Error("order: exception, events, logs of the request")
	}
}

// Each issue in the list can be opened from its title and from a button.
func TestIssueOpenButton(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	list := server.page("/issues")
	id := issueIDs(list)[0]

	if !strings.Contains(list, `<a class="button accent" href="/issues/`+id+`" title="Open the issue"`) {
		t.Fatal("the open button is missing")
	}
}

// requestEnvelopes builds an error and a log from one backend request, the
// way the Node SDK sends them: the log carries the request span as
// sentry.trace.parent_span_id, the event has it as parent_span_id.
func requestEnvelopes(traceID, handlerSpan, requestSpan, errorText, logText string) (event, logs []byte) {
	trace := fmt.Sprintf(`{"trace_id":%q,"span_id":%q,"parent_span_id":%q}`, traceID, handlerSpan, requestSpan)
	event = []byte(`{}` + "\n" + `{"type":"event"}` + "\n" +
		`{"exception":{"values":[{"type":"Error","value":` + strconv.Quote(errorText) + `}]},"contexts":{"trace":` + trace + `}}`)

	spanAttribute := ""

	if requestSpan != "" {
		spanAttribute = fmt.Sprintf(`,"attributes":{"sentry.trace.parent_span_id":{"type":"string","value":%q}}`, requestSpan)
	}

	logs = []byte(`{}` + "\n" + `{"type":"log","item_count":1}` + "\n" +
		fmt.Sprintf(`{"items":[{"level":"error","body":%q,"trace_id":%q%s}]}`, logText, traceID, spanAttribute))

	return event, logs
}

func TestLogsLinkedByRequestNotByTrace(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	// Two requests from one browser tab: same trace, different spans.
	const (
		trace  = "f52636a524ff4d968e1a56e205a2a1d6"
		target = fixtures.BackendEnvelopeURL
	)

	for _, request := range [][4]string{
		{"aaaaaaaaaaaaaaa1", "bbbbbbbbbbbbbbb1", "Order 298 has no items", "log of the order request"},
		{"aaaaaaaaaaaaaaa2", "bbbbbbbbbbbbbbb2", "Card declined by issuer", "log of the payment request"},
	} {
		event, logs := requestEnvelopes(trace, request[0], request[1], request[2], request[3])
		server.sendEnvelope(target, event)
		server.sendEnvelope(target, logs)
	}

	list := server.page("/issues?query=Order")
	orderIssue := issueIDs(list)[0]
	body := server.page("/issues/" + orderIssue)

	if !strings.Contains(body, "Logs from the same request") || !strings.Contains(body, "log of the order request") {
		t.Fatal("the issue page must show the log of its own request")
	}

	if strings.Contains(body, "log of the payment request") {
		t.Fatal("a log of another request in the same trace must not be shown")
	}

	// The logs page links each log to the issue of its request.
	logs := server.page("/logs")

	if !strings.Contains(logs, `href="/issues/`+orderIssue+`">Issue: Error: Order 298 has no items`) {
		t.Fatalf("log not linked to its issue:\n%s", logs)
	}
}

func TestRelatedLogsFallBackToTraceWithoutSpans(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	const target = fixtures.BackendEnvelopeURL

	// An SDK that sends no spans: only the trace links the log to the error.
	event, logs := requestEnvelopes("0123456789abcdef0123456789abcdef", "", "", "Export failed", "export log line")
	server.sendEnvelope(target, event)
	server.sendEnvelope(target, logs)

	body := server.page("/issues/" + issueIDs(server.page("/issues"))[0])

	if !strings.Contains(body, "Logs from the same trace") || !strings.Contains(body, "export log line") {
		t.Fatalf("expected the trace fallback:\n%s", body)
	}
}

func TestStatusChange(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	id := issueIDs(server.page("/issues"))[0]

	htmxResponse := server.post("/issues/"+id+"/action", url.Values{"action": {"resolve"}}, "HX-Request", "true")

	if htmxResponse.Header().Get("HX-Location") != "/issues/"+id {
		t.Fatalf("htmx status change: %d %v", htmxResponse.Code, htmxResponse.Header())
	}

	if strings.Contains(server.page("/issues"), `href="/issues/`+id+`"`) {
		t.Fatal("resolved issue still in the unresolved list")
	}

	if !strings.Contains(server.page("/issues?status=resolved"), `href="/issues/`+id+`"`) {
		t.Fatal("resolved issue missing from the resolved list")
	}

	if response := server.post("/issues/"+id+"/action", url.Values{"action": {"archive"}}); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: %d", response.Code)
	}

	if response := server.post("/issues/"+id+"/action", url.Values{"action": {"delete"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", response.Code)
	}

	if response := server.get("/issues/" + id); response.Code != http.StatusNotFound {
		t.Fatalf("deleted issue: %d", response.Code)
	}
}

func TestBulkIssueActions(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	list := server.page("/issues?project=frontend")
	ids := issueIDs(list)

	if len(ids) != 4 || !strings.Contains(list, `name="issue" value="`+ids[0]+`"`) {
		t.Fatalf("frontend list: %d issues, checkboxes present: %v", len(ids), strings.Contains(list, `name="issue"`))
	}

	// The list form returns to the same filtered list.
	if !strings.Contains(list, `name="return" value="/issues?project=frontend"`) {
		t.Fatal("bulk form must return to the current list")
	}

	bulk := func(action string, selected ...string) *httptest.ResponseRecorder {
		return server.post("/issues/bulk", url.Values{"action": {action}, "issue": selected, "return": {"/issues?project=frontend"}})
	}

	response := bulk("ignore", ids[0], ids[1])

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/issues?project=frontend" {
		t.Fatalf("bulk ignore: %d %q", response.Code, response.Header().Get("Location"))
	}

	if left := issueIDs(server.page("/issues?project=frontend")); len(left) != 2 {
		t.Fatalf("after ignoring 2 of 4: %d unresolved", len(left))
	}

	if ignored := issueIDs(server.page("/issues?project=frontend&status=ignored")); len(ignored) != 2 {
		t.Fatalf("ignored list: %d issues, want 2", len(ignored))
	}

	// Deleting includes an issue that is already gone: the rest is still processed.
	if response := bulk("delete", ids[2], "missing0"); response.Code != http.StatusSeeOther {
		t.Fatalf("bulk delete: %d", response.Code)
	}

	if response := server.get("/issues/" + ids[2]); response.Code != http.StatusNotFound {
		t.Fatalf("deleted issue: %d", response.Code)
	}

	if response := bulk("archive", ids[3]); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", response.Code)
	}

	if response := bulk("resolve", "not-a-number"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: %d", response.Code)
	}

	// return must stay on this site.
	response = server.post("/issues/bulk", url.Values{"action": {"reopen"}, "issue": {ids[0]}, "return": {"https://evil.test/"}})

	if response.Header().Get("Location") != "/issues" {
		t.Fatalf("return to another site: %q", response.Header().Get("Location"))
	}

	if response := server.post("/issues/bulk", url.Values{"action": {"resolve"}, "issue": {ids[3]}}, "Origin", "https://evil.test"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-site bulk action: %d", response.Code)
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

// Every issue action asks for confirmation before it is sent.
func TestIssueActionsAskForConfirmation(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	list := server.page("/issues")
	pages := map[string]string{"list": list, "issue page": server.page("/issues/" + issueIDs(list)[0])}
	button := regexp.MustCompile(`<button name="action"[^>]*>`)

	for name, body := range pages {
		buttons := button.FindAllString(body, -1)

		if len(buttons) == 0 {
			t.Fatalf("%s: no action buttons", name)
		}

		for _, tag := range buttons {
			if !strings.Contains(tag, "data-confirm=") {
				t.Errorf("%s: action button without confirmation: %s", name, tag)
			}
		}
	}
}

// Each row of the issues list shows its status next to the checkbox, so a
// mixed selection ("All" statuses) is readable.
func TestIssueRowsShowStatus(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	ids := issueIDs(server.page("/issues"))
	server.post("/issues/bulk", url.Values{"action": {"ignore"}, "issue": {ids[0]}, "return": {"/issues"}})
	server.post("/issues/bulk", url.Values{"action": {"resolve"}, "issue": {ids[1]}, "return": {"/issues"}})

	list := server.page("/issues?status=all")
	row := func(id string) string {
		start := strings.Index(list, `name="issue" value="`+id+`"`)

		if start < 0 {
			t.Fatalf("issue %s not in the list", id)
		}

		// The row starts before the checkbox.
		return list[strings.LastIndex(list[:start], "<tr"):start]
	}

	// The status is the row's look, explained on hover.
	for id, want := range map[string]string{
		ids[0]: `<tr class="issue-ignored" title="Ignored: no notifications, stays ignored">`,
		ids[1]: `<tr class="issue-resolved" title="Resolved: fixed, reopens if the error happens again">`,
		ids[2]: `<tr class="issue-unresolved" title="Unresolved: still happening or not looked at yet">`,
	} {
		if !strings.Contains(row(id), want) {
			t.Errorf("issue %s: %q missing in %q", id, want, row(id))
		}
	}
}
