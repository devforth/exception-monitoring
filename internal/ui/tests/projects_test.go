package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/docs"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
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

	// The DSN can be copied on the project page and in the list.
	if !strings.Contains(page, `data-copy="project-dsn"`) || !strings.Contains(server.page("/projects"), `data-copy="dsn-`+id+`"`) {
		t.Error("no DSN copy button")
	}

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

	projectID, _ := strconv.Atoi(id)
	ctx := context.Background()
	issues, _ := database.CountIssues(ctx, store.IssueFilter{ProjectIDs: []int{projectID}})
	logs, _, _ := database.CountLogs(ctx, store.LogFilter{ProjectIDs: []int{projectID}})
	events, _ := database.ListEvents(ctx, store.EventFilter{Limit: 1000})

	if issues != 0 || logs != 0 || slices.ContainsFunc(events, func(event store.EventSummary) bool { return event.ProjectID == projectID }) {
		t.Fatalf("data of the deleted project: %d issues, %d logs, events %v", issues, logs, events)
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

// Levels to record and Retention start closed, their headings showing the
// values set inside; a refused retention value opens its section.
func TestProjectFormSectionsShowTheirValues(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	if response := server.post("/projects/1", url.Values{"retention_events": {"90d"}, "event_level": {"fatal"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", response.Code, response.Body)
	}

	page := server.page("/projects/1")

	if strings.Contains(page, `<details class="form-section" open>`) {
		t.Error("a section is open")
	}

	summary := regexp.MustCompile(`(?s)<summary>\s*<span class="section-title">Levels to record</span>(.*?)</summary>`).FindStringSubmatch(page)

	if summary == nil || !strings.Contains(summary[1], ">fatal</span>") || strings.Contains(summary[1], ">error</span>") {
		t.Errorf("levels summary: %v", summary)
	}

	if !strings.Contains(page, `data-summary-field="retention_events" data-summary-label="events" data-summary-zero="forever">events 90d</span>`) ||
		!strings.Contains(page, `data-summary-field="retention_logs" data-summary-label="logs" data-summary-zero="forever" class="muted" title="Default">logs `) {
		t.Error("the retention summary does not show the own and the default values")
	}

	// Each field explains itself with the text of the docs: retention from
	// configuration.md, the others from ui.md.
	hint, _ := docs.Setting("RETENTION_EVENTS")

	if !strings.Contains(page, `title="`+hint+`"`) || strings.Count(page, `class="hint"`) != 9 {
		t.Error("the retention fields lack their hints")
	}

	refused := server.post("/projects/1", url.Values{"retention_logs": {"forever"}}).Body.String()

	if !strings.Contains(refused, `<details class="form-section" open>`) {
		t.Error("the section with a refused value stays closed")
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

// With PUBLIC_URL set, DSNs use it: SDKs may reach the server at another
// address than the one the UI was opened with.
func TestDSNUsesPublicURL(t *testing.T) {
	server := newTestServerWith(t, func(settings *config.Config) {
		settings.Notifications.PublicURL = "https://errors.example.com/monitoring"
	})
	server.login()

	if page := server.page("/projects"); !strings.Contains(page, "https://"+fixtures.BackendKey+"@errors.example.com/monitoring/1") {
		t.Fatalf("DSN:\n%s", page)
	}
}

// The Projects page shows each DSN and the storage section.
func TestProjectsPage(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	projects := server.page("/projects")

	if !strings.Contains(projects, "http://"+fixtures.BackendKey+"@example.com/1") {
		t.Fatalf("projects page must show the DSN:\n%s", projects)
	}

	// The storage section: size against the limit and what retention keeps.
	for _, want := range []string{" of 2.0 GB", "<meter", "By default events are kept 30 days, logs 1 day; all events per issue.", "has not happened for 365 days"} {
		if !strings.Contains(projects, want) {
			t.Errorf("projects page is missing %q", want)
		}
	}
}
