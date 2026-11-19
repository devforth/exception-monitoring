// Package ui is the composition root of the web interface (server-rendered
// HTML with htmx); the code lives in its sub-packages.
package ui

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/projects"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/controllers"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
	"github.com/devforth/exception-monitoring/internal/ui/routes"
	"github.com/devforth/exception-monitoring/internal/ui/session"
	"github.com/devforth/exception-monitoring/internal/ui/views"
)

type Handler struct {
	controllers *controllers.Controllers
	auth        *session.Auth
}

// New builds the UI on the services every part shares (services.New, built once in main).
func New(ctx context.Context, logger *slog.Logger, database store.Store, settings *config.Config, registry *projects.Registry, factory notify.Factory,
	all *services.All,
) (*Handler, error) {
	renderer, err := views.New(logger, registry, all.Issues, requests.Viewer, settings.AppName)

	if err != nil {
		return nil, err
	}

	// The first start creates the superadmin from env; later starts do not
	// use ADMIN_USERNAME/ADMIN_PASSWORD.
	if err := all.Users.EnsureSuperadmin(ctx, settings.AdminUsername, settings.AdminPassword); err != nil {
		return nil, err
	}

	auth := session.NewAuth(logger, database, settings, all.Users)

	return &Handler{
		controllers: controllers.New(controllers.Dependencies{
			Logger: logger, Registry: registry, Settings: settings, Factory: factory, Views: renderer, Auth: auth, Services: all,
		}),
		auth: auth,
	}, nil
}

// Register adds the UI routes. protect is the IP allowlist.
func (handler *Handler) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	routes.Register(router, protect, handler.controllers, handler.auth)
}
