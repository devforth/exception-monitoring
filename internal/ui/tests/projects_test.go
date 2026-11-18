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
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/store"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/platform/config"
)

var dsnPattern = regexp.MustCompile(`http://([0-9a-f]{32})@example\.com/(\d+)`)

// A project added in the UI gets a DSN that works at once, without a restart;
// its settings can be changed, and deleting it removes its data.
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
		"name": {"mobile"}, "color": {"#123456"},
		"event_level": {"fatal", "error"}, "log_level": {"error", "warn"},
	})

	if created.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}

	page := server.page(created.Header().Get("Location"))
	match := dsnPattern.FindStringSubmatch(page)

	if match == nil || !strings.Contains(page, `name="color" value="#123456"`) {
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

	// Levels left out go back to unchecked; the color is kept when not sent.
	if response := server.post("/projects/"+id, url.Values{"name": {"mobile-app"}, "event_level": {"error"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", response.Code, response.Body)
	}

	if page := server.page("/projects/" + id); !strings.Contains(page, `name="color" value="#123456"`) || !strings.Contains(page, `value="mobile-app"`) ||
		strings.Contains(page, `name="event_level" value="fatal" checked`) {
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
// only its color and levels, and it cannot be deleted in the UI.
func TestProjectFromEnv(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	page := server.page("/projects/1")

	if strings.Contains(page, `name="name"`) || strings.Contains(page, "Delete project") || !strings.Contains(page, fixtures.BackendKey) {
		t.Fatalf("project from env:\n%s", page)
	}

	if response := server.post("/projects/1", url.Values{"name": {"renamed"}, "color": {"#123456"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", response.Code, response.Body)
	}

	if page := server.page("/projects/1"); !strings.Contains(page, "<h1>backend</h1>") || !strings.Contains(page, `name="color" value="#123456"`) {
		t.Fatalf("after update:\n%s", page)
	}

	if response := server.post("/projects/1/delete", nil); response.Code != http.StatusConflict {
		t.Fatalf("delete from env: %d", response.Code)
	}

	if response := server.post("/projects/1", url.Values{"color": {"red"}}); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Choose a color") {
		t.Fatalf("invalid color: %d", response.Code)
	}
}

// Levels to record starts closed, its heading showing the values set inside.
func TestProjectFormSectionsShowTheirValues(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	if response := server.post("/projects/1", url.Values{"event_level": {"fatal"}}); response.Code != http.StatusSeeOther {
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

	// Each field explains itself with the text of ui.md.
	if strings.Count(page, `class="hint"`) != 4 {
		t.Errorf("hints: %d", strings.Count(page, `class="hint"`))
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

	// The storage section: size against the limit and how it is cleaned.
	for _, want := range []string{" of 2.0 GB", "<meter", "until the database is at 80%: old logs first, errors last"} {
		if !strings.Contains(projects, want) {
			t.Errorf("projects page is missing %q", want)
		}
	}
}

// The Projects page lists the latest cleanups; the steps of a run open in a popup.
func TestCleanupHistoryOnProjectsPage(t *testing.T) {
	database := openDatabase(t)
	cleaner, ok := database.(interface {
		Cleanup(ctx context.Context, maxBytes int64) (store.CleanupRun, error)
	})

	if !ok {
		t.Skip("the cleanup by size runs on the key-value store (STORE=kv)")
	}

	ctx := context.Background()

	if err := database.SaveLogs(ctx, []store.NewLog{{ProjectID: 1, Timestamp: time.Now(), Level: "info", Body: "a log to clean"}}, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := cleaner.Cleanup(ctx, 1); err != nil {
		t.Fatal(err)
	}

	server := startServer(t, database, "admin", password)
	server.login()
	page := server.page("/projects")

	for _, want := range []string{"Recent cleanups", "cleanup-done", `popovertarget="cleanup-`, "logs below error", `data-project="backend"`, "1 logs"} {
		if !strings.Contains(page, want) {
			t.Errorf("projects page is missing %q", want)
		}
	}
}
