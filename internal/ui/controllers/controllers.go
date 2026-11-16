// Package controllers holds the HTTP handlers of the UI, one file per page or
// group of pages. A handler does HTTP only: it reads the request (with
// requests), calls services, fills a view type of contracts and renders it
// with views. It never uses the store: all data access goes through services,
// and presenters turn the data into view types.
package controllers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/oauth"
	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/presenters"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
	"github.com/devforth/exception-monitoring/internal/ui/services"
	"github.com/devforth/exception-monitoring/internal/ui/views"
)

// Controllers holds what the handlers need.
type Controllers struct {
	logger        *slog.Logger
	registry      *projects.Registry
	notifications config.Notifications
	retention     config.Retention
	recording     config.Recording
	factory       notify.Factory
	views         *views.Renderer
	auth          *services.Auth
	issues        *services.Issues
	projects      *services.Projects
	connections   *services.Connections
	deliveries    *services.Deliveries
	verification  *services.Verification
	logs          *services.Logs
	agents        *services.Agents
	users         *services.Users
	// For the MCP page: whether ALLOWED_IPS limits access.
	ipsRestricted bool
	appName       string // APP_NAME

	connectionViews presenters.Connections
	projectViews    presenters.Projects
}

// Dependencies are the parts Controllers is built from.
type Dependencies struct {
	Logger   *slog.Logger
	Store    *store.Store
	Registry *projects.Registry
	Settings *config.Config
	Factory  notify.Factory
	Views    *views.Renderer
	Auth     *services.Auth
	OAuth    *oauth.Server // the MCP server's sign-in, shared with it
}

func New(dependencies Dependencies) *Controllers {
	settings := dependencies.Settings
	projectService := services.NewProjects(dependencies.Store, dependencies.Registry, settings.Notifications.PublicURL, settings.TrustedProxies)

	return &Controllers{
		logger:        dependencies.Logger,
		registry:      dependencies.Registry,
		notifications: settings.Notifications,
		retention:     settings.Retention,
		recording:     settings.Recording,
		factory:       dependencies.Factory,
		views:         dependencies.Views,
		auth:          dependencies.Auth,
		issues:        services.NewIssues(dependencies.Store),
		projects:      projectService,
		connections:   services.NewConnections(dependencies.Logger, dependencies.Store, dependencies.Factory),
		deliveries:    services.NewDeliveries(dependencies.Store),
		verification:  services.NewVerification(dependencies.Logger, dependencies.Store, dependencies.Factory),
		logs:          services.NewLogs(dependencies.Store),
		agents:        services.NewAgents(dependencies.OAuth, dependencies.Store),
		users:         services.NewUsers(dependencies.Logger, dependencies.Store),
		ipsRestricted: len(settings.AllowedIPs) > 0,
		appName:       settings.AppName,
		connectionViews: presenters.Connections{
			Factory: dependencies.Factory, Notifications: settings.Notifications,
			Projects: dependencies.Registry, ProjectName: projectService.Name,
		},
		projectViews: presenters.Projects{
			Retention: settings.Retention, Recording: settings.Recording,
			FromEnv: dependencies.Registry.FromEnv, Color: projectService.Color,
		},
	}
}

// Home sends the site root to the issues.
func (controller *Controllers) Home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/issues", http.StatusSeeOther)
}

func (controller *Controllers) serverError(w http.ResponseWriter, r *http.Request, err error) {
	controller.views.ServerError(w, r, err)
}

// NotFoundPage is the 404 page: for unknown addresses, missing records, and
// what the viewer's role does not allow, so it looks as if it did not exist.
// htmx requests get plain text: a page must not be swapped into a fragment.
func (controller *Controllers) NotFoundPage(w http.ResponseWriter, r *http.Request) {
	if requests.IsHTMX(r) {
		http.NotFound(w, r)

		return
	}

	controller.views.NotFound(w, r)
}

// pathID reads a positive id from the path value name; a 404 page
// otherwise (false).
func (controller *Controllers) pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)

	if err != nil || id <= 0 {
		controller.NotFoundPage(w, r)

		return 0, false
	}

	return id, true
}

// failed answers an error of a service call: 404 for a missing record or a
// change the role does not allow, 500 otherwise. False without an error.
func (controller *Controllers) failed(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound), errors.Is(err, services.ErrForbidden):
		controller.NotFoundPage(w, r)
	default:
		controller.serverError(w, r, err)
	}

	return true
}

// redirectAfterPost sends the browser to target after a form POST. htmx
// would swap a redirect response into the page, so it gets HX-Location
// (a client-side navigation) instead.
func redirectAfterPost(w http.ResponseWriter, r *http.Request, target string) {
	if requests.IsHTMX(r) {
		w.Header().Set("HX-Location", target)

		return
	}

	http.Redirect(w, r, target, http.StatusSeeOther)
}
