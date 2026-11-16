// Package views turns page data into HTML: it embeds the templates and the
// static files, parses the templates with their functions, versions asset
// URLs and renders full pages, htmx fragments and standalone partials.
package views

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
	"maps"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/api/agentprompt"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/projects"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/helpers"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

//go:embed templates
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// link is the data of a partial that is a link: where to and its text.
func link(url, label string) map[string]string { return map[string]string{"URL": url, "Label": label} }

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
	"retryNow": func(id any, count int, back string) map[string]any {
		return map[string]any{"ID": id, "Count": count, "Return": back}
	},
	"back":     link,
	"editLink": link,
	// The buttons of the issue-action-buttons partial: an issue's status, or "bulk".
	"roleSummary": func(role string) string { return store.RoleSummaries[role] },
	"issueActions": func(status string, canDelete bool) map[string]any {
		return map[string]any{"Status": status, "CanDelete": canDelete}
	},
	"limits":   func() contracts.FieldLimits { return contracts.Limits },
	"chatTest": func(url, needs string) map[string]string { return map[string]string{"URL": url, "Needs": needs} },
	"copyButton": func(target, label, title, copied string) map[string]string {
		return map[string]string{"Target": target, "Label": label, "Title": title, "Copied": copied}
	},
	"copyText": func(text, label, title, copied string) map[string]string {
		return map[string]string{"Text": text, "Label": label, "Title": title, "Copied": copied}
	},
	"projectBadge": lists.ProjectBadge,
	"levelBadge":   lists.LevelBadge,
	"statusBadge":  lists.StatusBadge,
	// label is a badge that is not a link (inside checkboxes): {{template "badge" (label "level" .Name)}}.
	"label": lists.Label,
}

// Templates: layout.html wraps every page, partials/ holds the pieces shared
// by pages, and every other file is a page, in a folder by section
// (issues/issue.html). A page is named after its file ("issue"), so names
// must be unique.
const (
	layoutFile   = "templates/layout.html"
	partialFiles = "templates/partials/*.html"
)

// parseTemplates parses every page with the layout, the partials, the
// shared functions and those that depend on the settings: appName
// (APP_NAME) and mcpReference (the MCP request of an issue, as
// notifications carry it).
func parseTemplates(appName string) (map[string]*template.Template, error) {
	pages := map[string]*template.Template{}
	funcs := maps.Clone(templateFuncs)
	funcs["appName"] = func() string { return appName }
	funcs["mcpReference"] = func(issueID int64) string { return agentprompt.MCPReference(appName, issueID) }

	pageFiles, err := fs.Glob(templateFiles, "templates/*/*.html")

	if err != nil {
		return nil, err
	}

	for _, file := range pageFiles {
		if strings.HasPrefix(file, "templates/partials/") {
			continue
		}

		name := strings.TrimSuffix(path.Base(file), ".html")

		if _, taken := pages[name]; taken {
			return nil, fmt.Errorf("two templates are named %s", name)
		}

		page, err := template.New(name).Funcs(funcs).ParseFS(templateFiles, layoutFile, partialFiles, file)

		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", file, err)
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
	Page string // active menu item
	Data any
	// ProjectColors is the URL of the stylesheet with the project colors.
	ProjectColors string
	// SidebarCollapsed: the menu shows icons only (the sidebar cookie, set by
	// app.js), rendered by the server so the page does not jump on load.
	SidebarCollapsed bool
	// UnresolvedIssues is the counter next to Issues in the menu.
	UnresolvedIssues int
	// Viewer is the logged-in user (the zero User without a session): the
	// menu shows what their role allows.
	Viewer store.User
}

// LoggedIn: the page is shown to a user (with the menu).
func (pageView view) LoggedIn() bool {
	return pageView.Viewer.ID > 0
}

const sidebarCookie = "em_sidebar"

// ProjectColors gives the stylesheet of the project colors (projects.Registry
// does); pages link it with its version, so it is fetched again only when a
// color changes.
type ProjectColors interface {
	Colors() projects.Stylesheet
}

// IssueCounter counts the unresolved issues for the menu (services.Issues does).
type IssueCounter interface {
	Unresolved(ctx context.Context) (int, error)
}

// ViewerOf returns the logged-in user of a request (requests.Viewer).
type ViewerOf func(r *http.Request) store.User

// Renderer executes the parsed templates.
type Renderer struct {
	logger *slog.Logger
	colors ProjectColors
	issues IssueCounter
	viewer ViewerOf
	pages  map[string]*template.Template
}

func New(logger *slog.Logger, colors ProjectColors, issues IssueCounter, viewer ViewerOf, appName string) (*Renderer, error) {
	if appName == "" {
		appName = config.DefaultAppName
	}

	pages, err := parseTemplates(appName)

	if err != nil {
		return nil, err
	}

	return &Renderer{logger: logger, colors: colors, issues: issues, viewer: viewer, pages: pages}, nil
}

// Render executes a full page (the "layout" template around the page's "content").
func (renderer *Renderer) Render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	pageView := view{Data: data, Page: page, Viewer: renderer.viewer(r)}

	if cookie, err := r.Cookie(sidebarCookie); err == nil {
		pageView.SidebarCollapsed = cookie.Value == "collapsed"
	}

	if pageView.LoggedIn() {
		pageView.ProjectColors = "/project-colors.css?v=" + renderer.colors.Colors().Version

		// The counter is a hint: a failed count leaves it out, not the page.
		count, err := renderer.issues.Unresolved(r.Context())

		if err != nil {
			renderer.logger.Warn("count unresolved issues", "err", err)
		}

		pageView.UnresolvedIssues = count
	}

	renderer.execute(w, r, status, page, "layout", pageView)
}

// RenderFragment executes one named template of a page, for htmx swaps.
func (renderer *Renderer) RenderFragment(w http.ResponseWriter, r *http.Request, page, fragment string, data any) {
	renderer.execute(w, r, http.StatusOK, page, fragment, view{Page: page, Data: data, Viewer: renderer.viewer(r)})
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

// NotFound renders the 404 page.
func (renderer *Renderer) NotFound(w http.ResponseWriter, r *http.Request) {
	renderer.Render(w, r, http.StatusNotFound, "not-found", nil)
}
