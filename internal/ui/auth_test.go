package ui_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
