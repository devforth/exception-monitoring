package ui_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

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

// The menu is rendered collapsed when the sidebar cookie says so.
func TestSidebarCollapsed(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	if page := server.page("/issues"); strings.Contains(page, "sidebar-collapsed") {
		t.Fatal("the menu starts expanded")
	}

	request := httptest.NewRequest(http.MethodGet, "/issues", nil)
	request.AddCookie(&http.Cookie{Name: "em_sidebar", Value: "collapsed"})

	if page := server.send(request).Body.String(); !strings.Contains(page, `class="app sidebar-collapsed"`) {
		t.Fatal("the cookie must collapse the menu")
	}
}

// Pages below a list link back to it with the one "back" partial, which
// app.js follows on Esc (data-back).
func TestBackLinks(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	for path, want := range map[string]string{
		"/issues/" + issueIDs(server.page("/issues"))[0]: `<a href="/issues" data-back`,
		"/projects/1":        `<a href="/projects" data-back`,
		"/notifications/new": `<a href="/notifications" data-back`,
		"/notifications/log": `<a href="/notifications" data-back`,
	} {
		if !strings.Contains(server.page(path), want) {
			t.Errorf("%s: missing %s", path, want)
		}
	}
}
