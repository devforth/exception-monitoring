// Package ui is the web interface: server-rendered HTML pages (html/template)
// with htmx swapping parts of a page (filters, "load more", status buttons)
// without a full reload. Templates, CSS and htmx are embedded in the binary,
// so the UI works without internet access.
//
// Access: the IP allowlist is applied by the server; this package adds the
// admin login (session cookie stored as a hash in SQLite) and CSRF checks.
package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

type Handler struct {
	logger        *slog.Logger
	store         *store.Store
	projects      config.Projects
	adminUsername string
	adminPassword string
	// credentials fingerprints the admin username and password; sessions are
	// valid only with the fingerprint they were created with.
	credentials    string
	trustedProxies []netip.Prefix
	limiter        *loginLimiter
	pages          map[string]*template.Template
	now            func() time.Time
}

func New(ctx context.Context, logger *slog.Logger, database *store.Store, settings *config.Config) (*Handler, error) {
	pages, err := parseTemplates()

	if err != nil {
		return nil, err
	}

	credentials, err := credentialsFingerprint(settings.AdminUsername, settings.AdminPassword)

	if err != nil {
		return nil, err
	}

	// Sessions from before a username or password change would be rejected
	// anyway; deleting them keeps the table clean.
	if err := database.DeleteSessionsExcept(ctx, credentials); err != nil {
		return nil, fmt.Errorf("drop old sessions: %w", err)
	}

	return &Handler{
		logger:         logger,
		store:          database,
		projects:       settings.Projects,
		adminUsername:  settings.AdminUsername,
		adminPassword:  settings.AdminPassword,
		credentials:    credentials,
		trustedProxies: settings.TrustedProxies,
		limiter:        newLoginLimiter(),
		pages:          pages,
		now:            time.Now,
	}, nil
}

// pageNames are the templates in templates/ that are full pages. Each is
// parsed together with layout.html and partials.html.
var pageNames = []string{"login", "issues", "issue", "logs", "projects"}

func parseTemplates() (map[string]*template.Template, error) {
	pages := map[string]*template.Template{}

	for _, name := range pageNames {
		page, err := template.New(name).Funcs(templateFuncs).ParseFS(templateFiles,
			"templates/layout.html", "templates/partials.html", "templates/"+name+".html")

		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}

		pages[name] = page
	}

	return pages, nil
}

// Register adds the UI routes. protect is the IP allowlist.
func (handler *Handler) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	static, _ := fs.Sub(staticFiles, "static")

	routes := map[string]http.HandlerFunc{
		"GET /{$}":                      handler.requireLogin(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/issues", http.StatusSeeOther) }),
		"GET /login":                    handler.loginPage,
		"POST /login":                   sameOrigin(handler.login),
		"POST /logout":                  sameOrigin(handler.logout),
		"GET /issues":                   handler.requireLogin(handler.issuesPage),
		"GET /issues/{issueID}":         handler.requireLogin(handler.issuePage),
		"POST /issues/{issueID}/action": handler.requireLogin(sameOrigin(handler.issueAction)),
		"POST /issues/bulk":             handler.requireLogin(sameOrigin(handler.bulkIssueAction)),
		"GET /logs":                     handler.requireLogin(handler.logsPage),
		"GET /projects":                 handler.requireLogin(handler.projectsPage),
		"GET /static/":                  http.StripPrefix("/static/", staticCache(http.FileServerFS(static))).ServeHTTP,
	}

	for pattern, route := range routes {
		router.Handle(pattern, protect(securityHeaders(route)))
	}
}

// staticCache: pages link assets with ?v=<content hash> (see assetURL), so a
// versioned URL never changes content and can be cached for good. Without the
// version, the browser must revalidate, or it keeps an old app.css after an update.
func staticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("v") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		next.ServeHTTP(w, r)
	})
}

// assetVersions maps a file in static/ to a short hash of its content.
var assetVersions = func() map[string]string {
	versions := map[string]string{}
	entries, _ := staticFiles.ReadDir("static")

	for _, entry := range entries {
		content, err := staticFiles.ReadFile("static/" + entry.Name())

		if err != nil {
			continue
		}

		sum := sha256.Sum256(content)
		versions[entry.Name()] = hex.EncodeToString(sum[:])[:12]
	}

	return versions
}()

// assetURL is the versioned URL of a static file, for templates: {{asset "app.css"}}.
func assetURL(name string) string {
	return "/static/" + name + "?v=" + assetVersions[name]
}

// securityHeaders: scripts and styles only from our own /static/ (no inline
// code), no framing by other sites (clickjacking), no MIME sniffing.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		header.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// view is what every page template receives.
type view struct {
	Page     string // active menu item
	LoggedIn bool
	Data     any
}

// render executes a full page (the "layout" template around the page's "content").
func (handler *Handler) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	handler.execute(w, r, status, page, "layout", view{Page: page, LoggedIn: page != "login", Data: data})
}

// renderFragment executes one named template of a page, for htmx swaps.
func (handler *Handler) renderFragment(w http.ResponseWriter, r *http.Request, page, fragment string, data any) {
	handler.execute(w, r, http.StatusOK, page, fragment, view{Page: page, LoggedIn: true, Data: data})
}

func (handler *Handler) execute(w http.ResponseWriter, r *http.Request, status int, page, name string, data view) {
	// Render into a buffer first: a template error must not leave a half-written page.
	var buffer bytes.Buffer

	if err := handler.pages[page].ExecuteTemplate(&buffer, name, data); err != nil {
		handler.serverError(w, r, fmt.Errorf("render %s/%s: %w", page, name, err))

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buffer.WriteTo(w)
}

func (handler *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	handler.logger.Error("ui request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Internal error", http.StatusInternalServerError)
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
