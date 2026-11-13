// Package views turns page data into HTML: it embeds the templates and the
// static files, parses the templates with their functions, versions asset
// URLs and renders full pages, htmx fragments and standalone partials.
package views

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/helpers"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

var templateFuncs = template.FuncMap{
	"timeAgo":      helpers.TimeAgo,
	"isoTime":      func(value time.Time) string { return value.UTC().Format(time.RFC3339) },
	"fullTime":     func(value time.Time) string { return value.UTC().Format("2006-01-02 15:04:05.000 UTC") },
	"levelClass":   helpers.LevelClass,
	"cleanLog":     helpers.CleanLog,
	"prettyJSON":   helpers.PrettyJSON,
	"handledLabel": helpers.HandledLabel,
	"parseRequest": contracts.ParseRequest,
	"short":        helpers.Short,
	"asset":        assetURL,
	"back":         func(url, label string) map[string]string { return map[string]string{"URL": url, "Label": label} },
	"projectBadge": lists.ProjectBadge,
	"levelBadge":   lists.LevelBadge,
	"statusBadge":  lists.StatusBadge,
	// label is a badge that is not a link (inside checkboxes): {{template "badge" (label "level" .Name)}}.
	"label": lists.Label,
}

// pageNames are the templates in templates/ that are full pages. Each is
// parsed together with layout.html and partials.html.
var pageNames = []string{"login", "issues", "issue", "logs", "projects", "project", "notifications", "connection", "deliveries"}

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

// Static is the content of static/, served under /static/.
func Static() fs.FS {
	static, _ := fs.Sub(staticFiles, "static")

	return static
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

// view is what every page template receives.
type view struct {
	Page     string // active menu item
	LoggedIn bool
	Data     any
	// ProjectColors is the URL of the stylesheet with the project colors.
	ProjectColors string
	// SidebarCollapsed: the menu shows icons only (the sidebar cookie, set by
	// app.js), rendered by the server so the page does not jump on load.
	SidebarCollapsed bool
}

const sidebarCookie = "em_sidebar"

// ProjectColors gives the stylesheet of the project colors (projects.Registry
// does); pages link it with its version, so it is fetched again only when a
// color changes.
type ProjectColors interface {
	Colors() projects.Stylesheet
}

// Renderer executes the parsed templates.
type Renderer struct {
	logger *slog.Logger
	colors ProjectColors
	pages  map[string]*template.Template
}

func New(logger *slog.Logger, colors ProjectColors) (*Renderer, error) {
	pages, err := parseTemplates()

	if err != nil {
		return nil, err
	}

	return &Renderer{logger: logger, colors: colors, pages: pages}, nil
}

// Render executes a full page (the "layout" template around the page's "content").
func (renderer *Renderer) Render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	pageView := view{Page: page, LoggedIn: page != "login", Data: data}

	if cookie, err := r.Cookie(sidebarCookie); err == nil {
		pageView.SidebarCollapsed = cookie.Value == "collapsed"
	}

	if pageView.LoggedIn {
		pageView.ProjectColors = "/project-colors.css?v=" + renderer.colors.Colors().Version
	}

	renderer.execute(w, r, status, page, "layout", pageView)
}

// RenderFragment executes one named template of a page, for htmx swaps.
func (renderer *Renderer) RenderFragment(w http.ResponseWriter, r *http.Request, page, fragment string, data any) {
	renderer.execute(w, r, http.StatusOK, page, fragment, view{Page: page, LoggedIn: true, Data: data})
}

// RenderPartial executes a template of a page with data as is (not wrapped
// in view), for fragments that stand alone such as the email step.
func (renderer *Renderer) RenderPartial(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	renderer.execute(w, r, http.StatusOK, page, name, data)
}

func (renderer *Renderer) execute(w http.ResponseWriter, r *http.Request, status int, page, name string, data any) {
	// Render into a buffer first: a template error must not leave a half-written page.
	var buffer bytes.Buffer

	if err := renderer.pages[page].ExecuteTemplate(&buffer, name, data); err != nil {
		renderer.ServerError(w, r, fmt.Errorf("render %s/%s: %w", page, name, err))

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buffer.WriteTo(w)
}

// ServerError logs a failed request and answers 500.
func (renderer *Renderer) ServerError(w http.ResponseWriter, r *http.Request, err error) {
	renderer.logger.Error("ui request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Internal error", http.StatusInternalServerError)
}
