package tests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/platform/config"
)

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

// ADMIN_USERNAME/ADMIN_PASSWORD create the superadmin at the first start
// only: later starts with other values change nothing, and sessions survive
// restarts.
func TestSuperadminFromEnvOnce(t *testing.T) {
	database := openDatabase(t)

	first := startServer(t, database, "admin", password)
	first.login()

	for _, credentials := range [][2]string{{"admin", password}, {"admin", "another password"}, {"root", "another password"}} {
		restarted := startServer(t, database, credentials[0], credentials[1])
		restarted.cookie = first.cookie

		if response := restarted.get("/issues"); response.Code != http.StatusOK {
			t.Fatalf("restart with %q: %d", credentials[0], response.Code)
		}

		if response := restarted.post("/login", url.Values{"username": {credentials[0]}, "password": {"another password"}}); response.Code != http.StatusUnauthorized {
			t.Fatalf("the env password of a restart logs in: %d", response.Code)
		}
	}

	if users := first.page("/users"); strings.Count(users, `href="/users/`) != 1 || strings.Contains(users, ">root<") {
		t.Errorf("a restart created a user:\n%s", users)
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

// Guessing one account's password from many addresses is limited too: the
// username is blocked whatever address the next attempt comes from.
func TestLoginRateLimitPerUsername(t *testing.T) {
	server := newTestServerWith(t, func(settings *config.Config) {
		settings.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")} // httptest's peer
	})

	from := func(index int) []string {
		return []string{"X-Forwarded-For", fmt.Sprintf("203.0.113.%d", index)}
	}

	for index := range 10 {
		server.post("/login", url.Values{"username": {"Admin"}, "password": {"guess guess guess"}}, from(index)...)
	}

	if response := server.post("/login", url.Values{"username": {"admin"}, "password": {password}}, from(50)...); response.Code != http.StatusTooManyRequests {
		t.Fatalf("right password from a new address: %d, want 429", response.Code)
	}

	// Another username from that address is not blocked.
	if response := server.post("/login", url.Values{"username": {"dev"}, "password": {"guess guess guess"}}, from(50)...); response.Code == http.StatusTooManyRequests {
		t.Fatal("another username is blocked")
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

// Every dashboard route asks for a login, pages and actions alike; only the
// login itself and static files are open.
func TestDashboardNeedsLogin(t *testing.T) {
	server := newTestServer(t, password)

	for _, path := range []string{"/", "/issues", "/issues/1", "/logs", "/projects", "/projects/1", "/notifications", "/notifications/log", "/notifications/1", "/project-colors.css"} {
		if response := server.get(path); response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/login") {
			t.Errorf("GET %s: %d %q", path, response.Code, response.Header().Get("Location"))
		}
	}

	for _, path := range []string{"/issues/bulk", "/projects", "/projects/1/delete", "/notifications", "/notifications/1/retry", "/notifications/email/start"} {
		if response := server.post(path, url.Values{}); response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/login") {
			t.Errorf("POST %s: %d %q", path, response.Code, response.Header().Get("Location"))
		}
	}

	if response := server.get("/login"); response.Code != http.StatusOK {
		t.Errorf("the login page must be open: %d", response.Code)
	}
}
