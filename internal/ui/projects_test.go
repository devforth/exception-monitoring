package ui_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/fixtures"
)

var dsnPattern = regexp.MustCompile(`http://([0-9a-f]{32})@example\.com/(\d+)`)

// A project added in the UI gets a DSN that works at once, without a restart;
// its retention can be set, and deleting it removes its data.
func TestProjectLifecycle(t *testing.T) {
	database := openDatabase(t)
	server := startServer(t, database, "admin", password)
	server.login()

	if response := server.post("/projects", url.Values{"name": {"Not Valid"}}); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "lowercase letters") {
		t.Fatalf("invalid name: %d", response.Code)
	}

	if response := server.post("/projects", url.Values{"name": {"backend"}}); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "already has this name") {
		t.Fatalf("taken name: %d", response.Code)
	}

	created := server.post("/projects", url.Values{
		"name": {"mobile"}, "retention_logs": {"3d"}, "retention_logs_per_message": {"10"},
		"event_level": {"fatal", "error"}, "log_level": {"error", "warn"},
	})

	if created.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}

	page := server.page(created.Header().Get("Location"))
	match := dsnPattern.FindStringSubmatch(page)

	if match == nil || !strings.Contains(page, `name="retention_logs" value="3d"`) || !strings.Contains(page, `name="retention_logs_per_message" value="10"`) {
		t.Fatalf("new project page:\n%s", page)
	}

	key, id := match[1], match[2]

	// The DSN works right away.
	envelope := []byte(`{}` + "\n" + `{"type":"event"}` + "\n" + `{"message":"from mobile"}`)
	server.sendEnvelope("/api/"+id+"/envelope/?sentry_key="+key, envelope)

	if !strings.Contains(server.page("/issues?project=mobile"), "from mobile") {
		t.Fatal("the event of the new project is not listed")
	}

	// Levels the project does not record are dropped at ingest.
	server.sendEnvelope("/api/"+id+"/envelope/?sentry_key="+key, []byte(`{}`+"\n"+`{"type":"event"}`+"\n"+`{"message":"mobile info","level":"info"}`))

	if strings.Contains(server.page("/issues?project=mobile&status=all"), "mobile info") {
		t.Fatal("an info event must be dropped when only error and fatal are recorded")
	}

	if page := server.page("/projects/" + id); !strings.Contains(page, `name="event_level" value="error" checked`) || strings.Contains(page, `name="event_level" value="info" checked`) ||
		!strings.Contains(page, `name="log_level" value="warn" checked`) || strings.Contains(page, `name="log_level" value="info" checked`) {
		t.Fatal("level checkboxes")
	}

	if list := server.page("/projects"); !strings.Contains(list, ">logs 3 days, 10 logs per message</td>") {
		t.Fatalf("projects list:\n%s", list)
	}

	// Empty fields go back to the defaults.
	if response := server.post("/projects/"+id, url.Values{"name": {"mobile-app"}, "event_level": {"error"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", response.Code, response.Body)
	}

	if page := server.page("/projects/" + id); !strings.Contains(page, `name="retention_logs" value=""`) || !strings.Contains(page, `value="mobile-app"`) {
		t.Fatalf("after update:\n%s", page)
	}

	if response := server.post("/projects/"+id+"/delete", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", response.Code)
	}

	var left int

	if err := database.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM events WHERE project_id = ?) + (SELECT COUNT(*) FROM issues WHERE project_id = ?)`, id, id).Scan(&left); err != nil || left != 0 {
		t.Fatalf("data of the deleted project: %d (%v)", left, err)
	}

	// Its DSN stops working.
	response := httptest.NewRecorder()
	server.handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/"+id+"/envelope/?sentry_key="+key, strings.NewReader(string(envelope))))

	if response.Code != http.StatusForbidden {
		t.Fatalf("DSN of a deleted project: %d", response.Code)
	}
}

// A project from PROJECTS keeps the name and key set there: the form changes
// only its retention, and it cannot be deleted in the UI.
func TestProjectFromEnv(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	page := server.page("/projects/1")

	if strings.Contains(page, `name="name"`) || strings.Contains(page, "Delete project") || !strings.Contains(page, fixtures.BackendKey) {
		t.Fatalf("project from env:\n%s", page)
	}

	if response := server.post("/projects/1", url.Values{"name": {"renamed"}, "retention_events": {"90d"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", response.Code, response.Body)
	}

	if page := server.page("/projects/1"); !strings.Contains(page, "<h1>backend</h1>") || !strings.Contains(page, `name="retention_events" value="90d"`) {
		t.Fatalf("after update:\n%s", page)
	}

	if response := server.post("/projects/1/delete", nil); response.Code != http.StatusConflict {
		t.Fatalf("delete from env: %d", response.Code)
	}

	if response := server.post("/projects/1", url.Values{"retention_events": {"forever"}}); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Enter a period") {
		t.Fatalf("invalid retention: %d", response.Code)
	}
}

// A project color comes as a stylesheet (the CSP blocks inline styles): the
// palette color until one is chosen, then the chosen one.
func TestProjectColor(t *testing.T) {
	server := newTestServer(t, password)

	// The stylesheet names the projects: only for logged-in users.
	if response := server.get("/project-colors.css"); response.Code != http.StatusSeeOther {
		t.Fatalf("anonymous: %d", response.Code)
	}

	server.login()

	stylesheet := regexp.MustCompile(`href="(/project-colors\.css\?v=[0-9a-f]+)"`)
	page := server.page("/issues")
	before := stylesheet.FindStringSubmatch(page)

	if before == nil {
		t.Fatal("pages must link the project colors")
	}

	if css := server.page(before[1]); !strings.Contains(css, `[data-project="backend"] { --project-color: #2563eb; }`) {
		t.Fatalf("palette color:\n%s", css)
	}

	if response := server.post("/projects/1", url.Values{"color": {"#ABCDEF"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("set color: %d %s", response.Code, response.Body)
	}

	after := stylesheet.FindStringSubmatch(server.page("/issues"))

	if after[1] == before[1] {
		t.Fatal("the stylesheet URL must change with the colors, or browsers keep the old one")
	}

	if css := server.page(after[1]); !strings.Contains(css, `[data-project="backend"] { --project-color: #abcdef; }`) {
		t.Fatalf("chosen color:\n%s", css)
	}

	if response := server.post("/projects/1", url.Values{"color": {"red; background: url(x)"}}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid color: %d", response.Code)
	}

	// Choosing the palette color again goes back to following the palette.
	server.post("/projects/1", url.Values{"color": {"#2563eb"}})

	if page := server.page("/projects/1"); !strings.Contains(page, `type="color" name="color" value="#2563eb"`) {
		t.Fatal("color input")
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
