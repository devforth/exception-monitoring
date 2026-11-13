// Package controllers holds the HTTP handlers of the UI, one file per page or
// group of pages. A handler does HTTP only: it reads the request (with
// requests), calls services, fills a view type of contracts and renders it
// with views. It never uses the store: all data access goes through services,
// and presenters turn the data into view types.
package controllers

import (
	"log/slog"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/notify"
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
