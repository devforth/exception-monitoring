// Package ui is the web interface: server-rendered HTML pages (html/template)
// with htmx swapping parts of a page (filtered and paged lists, issue
// actions, the email step of a connection) without a full reload. Templates,
// CSS and htmx are embedded in the binary, so the UI works without internet
// access.
//
// Access: the IP allowlist is applied by the server; this package adds the
// users' login (session cookie stored as a hash in SQLite), their roles and
// CSRF checks.
//
// This package is only the composition root; the code lives in sub-packages
// by role: contracts (view types), lists, helpers, views, middleware,
// requests, services, controllers and routes.
package ui

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/oauth"
	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/controllers"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
	"github.com/devforth/exception-monitoring/internal/ui/routes"
	"github.com/devforth/exception-monitoring/internal/ui/services"
	"github.com/devforth/exception-monitoring/internal/ui/views"
)

type Handler struct {
	controllers *controllers.Controllers
	auth        *services.Auth
}

func New(ctx context.Context, logger *slog.Logger, database *store.Store, settings *config.Config, registry *projects.Registry, factory notify.Factory,
	authorization *oauth.Server,
) (*Handler, error) {
	renderer, err := views.New(logger, registry, services.NewIssues(database), requests.Viewer, settings.AppName)

	if err != nil {
		return nil, err
	}

	// The first start creates the superadmin from env; later starts do not
	// use ADMIN_USERNAME/ADMIN_PASSWORD.
	if err := services.NewUsers(logger, database).EnsureSuperadmin(ctx, settings.AdminUsername, settings.AdminPassword); err != nil {
		return nil, err
	}

	auth := services.NewAuth(logger, database, settings.TrustedProxies)

	return &Handler{
		controllers: controllers.New(controllers.Dependencies{
			Logger: logger, Store: database, Registry: registry, Settings: settings, Factory: factory, Views: renderer, Auth: auth, OAuth: authorization,
		}),
		auth: auth,
	}, nil
}

// Register adds the UI routes. protect is the IP allowlist.
func (handler *Handler) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	routes.Register(router, protect, handler.controllers, handler.auth)
}
