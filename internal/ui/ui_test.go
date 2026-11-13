// Tests run the whole server (ingest + UI) against a real database filled by
// replaying the recorded SDK requests, and drive the UI like a browser.
package ui_test

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"slices"
	"strconv"
	"time"

	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ingest"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/secret"
	"github.com/devforth/exception-monitoring/internal/server"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui"
)

const (
	password = "correct horse battery"
)

// testLog sends server logs to t.Log: they are printed only for failed tests.
type testLog struct{ t *testing.T }

func (log testLog) Write(data []byte) (int, error) {
	log.t.Log(strings.TrimSpace(string(data)))

	return len(data), nil
}

type testServer struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
	// mails receives what the server sent through its (fake) SMTP server.
	mails <-chan fixtures.SMTPSession
}

const secretKey = "0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T, adminPassword string) *testServer {
	t.Helper()

	return startServer(t, openDatabase(t), "admin", adminPassword)
}

func openDatabase(t *testing.T) *store.Store {
	t.Helper()

	database, err := store.Open(context.Background(), t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})

	return database
}

// startServer builds a server on an existing database, like a restart of the
// real server with possibly different settings.
// newTestServerWith is newTestServer with settings changed by configure.
func newTestServerWith(t *testing.T, configure func(settings *config.Config)) *testServer {
	t.Helper()

	return startServer(t, openDatabase(t), "admin", password, configure)
}

func startServer(t *testing.T, database *store.Store, adminUsername, adminPassword string, configure ...func(settings *config.Config)) *testServer {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(testLog{t}, nil))
	smtp, mails := fixtures.FakeSMTP(t)
	settings := &config.Config{
		Projects:       fixtures.Projects(),
		AllowedOrigins: []string{"*"},
		AdminUsername:  adminUsername,
		AdminPassword:  adminPassword,
		// Every connection type is available: SMTP for email, a key for chat tokens.
		Notifications: config.Notifications{ChannelTypes: config.AllChannelTypes, SecretKey: secretKey, SMTP: smtp},
		// Shown on the Projects page; nothing is deleted by the UI.
		Retention: config.Retention{Events: 30 * 24 * time.Hour, Logs: 24 * time.Hour, MaxDatabaseBytes: 2 << 30, Issues: 365 * 24 * time.Hour},
	}

	for _, change := range configure {
		change(settings)
	}

	box, err := secret.New(secretKey)

	if err != nil {
		t.Fatal(err)
	}

	registry, err := projects.Open(context.Background(), logger, database, settings.Projects)

	if err != nil {
		t.Fatal(err)
	}

	// A fake Telegram API: tests never reach the real one.
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getChat") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":-100,"type":"group","title":"Ops chat"}}`))

			return
		}

		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(telegram.Close)

	webUI, err := ui.New(context.Background(), logger, database, settings, registry, notify.Factory{SMTP: smtp, Box: box, TelegramURL: telegram.URL})

	if err != nil {
		t.Fatal(err)
	}

	handler := server.New(server.Options{
		Logger:   logger,
		Database: database,
		Ingest:   ingest.New(logger, settings, registry, ingest.StoreSink{Store: database, Logger: logger}),
		UI:       webUI,
	}).Handler()

	return &testServer{t: t, handler: handler, mails: mails}
}

func (server *testServer) send(request *http.Request) *httptest.ResponseRecorder {
	if server.cookie != nil {
		request.AddCookie(server.cookie)
	}

	response := httptest.NewRecorder()
	server.handler.ServeHTTP(response, request)

	return response
}

func (server *testServer) get(target string) *httptest.ResponseRecorder {
	return server.send(httptest.NewRequest(http.MethodGet, target, nil))
}

func (server *testServer) post(target string, form url.Values, headers ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for index := 0; index+1 < len(headers); index += 2 {
		request.Header.Set(headers[index], headers[index+1])
	}

	return server.send(request)
}

// page fetches target as a logged-in user and fails unless it is 200.
func (server *testServer) page(target string) string {
	server.t.Helper()

	response := server.get(target)

	if response.Code != http.StatusOK {
		server.t.Fatalf("GET %s: %d %s", target, response.Code, response.Body)
	}

	return response.Body.String()
}

func (server *testServer) login() {
	server.t.Helper()

	response := server.post("/login", url.Values{"username": {"admin"}, "password": {password}, "next": {"/issues"}})

	if response.Code != http.StatusSeeOther {
		server.t.Fatalf("login: %d %s", response.Code, response.Body)
	}

	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "em_session" && cookie.Value != "" {
			server.cookie = cookie
		}
	}

	if server.cookie == nil {
		server.t.Fatal("login did not set a session cookie")
	}
}

func (server *testServer) ingestFixtures() {
	server.t.Helper()

	for _, fixture := range fixtures.All(server.t) {
		response := httptest.NewRecorder()
		server.handler.ServeHTTP(response, fixture.HTTP())

		if response.Code != http.StatusOK {
			server.t.Fatalf("%s: ingest got %d %s", fixture.Name, response.Code, response.Body)
		}
	}
}

func (server *testServer) sendEnvelope(target string, body []byte) {
	server.t.Helper()

	response := httptest.NewRecorder()
	server.handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body)))

	if response.Code != http.StatusOK {
		server.t.Fatalf("ingest %s: %d %s", target, response.Code, response.Body)
	}
}

var issueLink = regexp.MustCompile(`href="/issues/(\d+)"`)

// issueIDs lists the issues linked from a page, each once and in order (a
// list row links its issue from the title and from the open button).
func issueIDs(html string) []string {
	var ids []string

	for _, match := range issueLink.FindAllStringSubmatch(html, -1) {
		if !slices.Contains(ids, match[1]) {
			ids = append(ids, match[1])
		}
	}

	return ids
}

func TestLogin(t *testing.T) {
	server := newTestServer(t, password)

	if response := server.get("/issues"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login?next=%2Fissues" {
		t.Fatalf("anonymous: %d %q", response.Code, response.Header().Get("Location"))
	}

	htmx := httptest.NewRequest(http.MethodGet, "/issues?status=all", nil)
	htmx.Header.Set("HX-Request", "true")

	if response := server.send(htmx); response.Code != http.StatusUnauthorized || !strings.HasPrefix(response.Header().Get("HX-Redirect"), "/login") {
		t.Fatalf("anonymous htmx: %d %q", response.Code, response.Header().Get("HX-Redirect"))
	}

	if response := server.post("/login", url.Values{"username": {"admin"}, "password": {"wrong password!"}}); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", response.Code)
	}

	server.login()

	if !strings.Contains(server.page("/issues"), "<h1>Issues</h1>") {
		t.Fatal("issues page after login")
	}

	cookie := server.cookie

	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: %+v", cookie)
	}

	if response := server.post("/logout", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", response.Code)
	}

	// The old cookie no longer works: the session is deleted on the server.
	server.cookie = cookie

	if response := server.get("/issues"); response.Code != http.StatusSeeOther {
		t.Fatalf("after logout: %d", response.Code)
	}
}

func TestSessionsFollowCredentials(t *testing.T) {
	database := openDatabase(t)

	first := startServer(t, database, "admin", password)
	first.login()

	// A restart with the same credentials keeps the session.
	restarted := startServer(t, database, "admin", password)
	restarted.cookie = first.cookie

	if response := restarted.get("/issues"); response.Code != http.StatusOK {
		t.Fatalf("same credentials after restart: %d", response.Code)
	}

	// Changing the password or the username logs everyone out.
	for _, credentials := range [][2]string{{"admin", "another password"}, {"root", password}} {
		changed := startServer(t, database, credentials[0], credentials[1])
		changed.cookie = first.cookie

		if response := changed.get("/issues"); response.Code != http.StatusSeeOther {
			t.Fatalf("after changing to %q: %d, want a redirect to login", credentials[0], response.Code)
		}
	}
}

func TestLoginRedirectStaysOnSite(t *testing.T) {
	server := newTestServer(t, password)

	for next, want := range map[string]string{
		"/issues/5":          "/issues/5",
		"https://evil.test/": "/issues",
		"//evil.test/":       "/issues",
		"/\\evil.test":       "/issues",
	} {
		response := server.post("/login", url.Values{"username": {"admin"}, "password": {password}, "next": {next}})

		if location := response.Header().Get("Location"); location != want {
			t.Errorf("next=%q: redirected to %q, want %q", next, location, want)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	server := newTestServer(t, password)

	for range 10 {
		server.post("/login", url.Values{"username": {"admin"}, "password": {"guess guess guess"}})
	}

	// Even the right password is refused while the address is blocked.
	response := server.post("/login", url.Values{"username": {"admin"}, "password": {password}})

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", response.Code)
	}
}

func TestUIDisabledWithoutPassword(t *testing.T) {
	server := newTestServer(t, "")

	if body := server.page("/login"); !strings.Contains(body, "ADMIN_PASSWORD") {
		t.Fatal("login page must explain that the UI is disabled")
	}

	if response := server.post("/login", url.Values{"username": {"admin"}, "password": {""}}); response.Code != http.StatusForbidden {
		t.Fatalf("login with empty password: %d", response.Code)
	}
}

func TestCrossSiteRequestsRejected(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	id := issueIDs(server.page("/issues"))[0]

	response := server.post("/issues/"+id+"/action", url.Values{"action": {"resolve"}}, "Origin", "https://evil.test")

	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-site status change: %d", response.Code)
	}

	response = server.post("/issues/"+id+"/action", url.Values{"action": {"resolve"}}, "Origin", "http://example.com")

	if response.Code != http.StatusSeeOther {
		t.Fatalf("same-site status change: %d", response.Code)
	}
}

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

	if !strings.Contains(body, `type="button" data-copy-prompt="agent-fix-prompt"`) {
		t.Fatal("copy prompt button must not submit the issue action form")
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

func TestLogsAndProjectsPages(t *testing.T) {
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

func TestStaticAndSecurityHeaders(t *testing.T) {
	server := newTestServer(t, password)

	for _, path := range []string{"/static/htmx.min.js", "/static/app.css", "/static/app.js", "/static/flatpickr.min.js", "/static/flatpickr.min.css"} {
		response := server.get(path)

		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}

	response := server.get("/login")

	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}

	if response.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("pages must not be framed")
	}
	// Pages hold error data: no cache keeps them; static files stay cacheable.
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Vary"), "HX-Request") {
		t.Fatalf("page cache headers: %q, Vary %q", response.Header().Get("Cache-Control"), response.Header().Get("Vary"))
	}

	if cache := server.get(assetPath(server.page("/login"), "app.css")).Header().Get("Cache-Control"); !strings.Contains(cache, "immutable") {
		t.Fatalf("versioned static file: Cache-Control %q", cache)
	}
}

// assetPath finds the versioned URL of a static file in a page.
func assetPath(page, name string) string {
	match := regexp.MustCompile(`/static/` + regexp.QuoteMeta(name) + `\?v=[0-9a-f]+`).FindString(page)

	return strings.ReplaceAll(match, "&amp;", "&")
}

// Logging in again ends the session the browser had: a session cookie set
// by someone else (fixation) is never kept.
func TestLoginReplacesSession(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	previous := server.cookie
	server.login()

	if server.cookie.Value == previous.Value {
		t.Fatal("login must issue a new session token")
	}

	server.cookie = previous

	if response := server.get("/issues"); response.Code != http.StatusSeeOther {
		t.Fatalf("the previous session still works: %d", response.Code)
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

func TestStaticAssetsAreVersioned(t *testing.T) {
	server := newTestServer(t, password)

	page := server.page("/login")
	link := regexp.MustCompile(`href="(/static/app\.css\?v=[0-9a-f]{12})"`).FindStringSubmatch(page)

	if link == nil {
		t.Fatalf("app.css is not linked with a content version:\n%s", page)
	}

	for _, asset := range []string{"flatpickr.min.js", "flatpickr.min.css"} {
		if !regexp.MustCompile(`/static/` + regexp.QuoteMeta(asset) + `\?v=[0-9a-f]{12}`).MatchString(page) {
			t.Errorf("%s is not linked with a content version", asset)
		}
	}

	if cache := server.get(link[1]).Header().Get("Cache-Control"); !strings.Contains(cache, "immutable") {
		t.Fatalf("versioned asset Cache-Control = %q", cache)
	}

	// Without a version the browser must revalidate, or it keeps old CSS after an update.
	if cache := server.get("/static/app.css").Header().Get("Cache-Control"); cache != "no-cache" {
		t.Fatalf("unversioned asset Cache-Control = %q", cache)
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
	if response := bulk("delete", ids[2], "999999"); response.Code != http.StatusSeeOther {
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

		return list[start : start+300]
	}

	for id, want := range map[string]string{ids[0]: `aria-label="ignored">⊘`, ids[1]: `aria-label="resolved">✓`, ids[2]: `aria-label="unresolved">●`} {
		if !strings.Contains(row(id), want) {
			t.Errorf("issue %s: status icon %q missing in %q", id, want, row(id))
		}
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
