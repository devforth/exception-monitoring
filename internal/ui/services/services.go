package services

import (
	"log/slog"

	"github.com/devforth/exception-monitoring/internal/api/oauth"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/projects"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Dependencies are what the services are built from.
type Dependencies struct {
	Logger   *slog.Logger
	Store    *store.Store
	Registry *projects.Registry
	Settings *config.Config
	Factory  notify.Factory
	OAuth    *oauth.Server // the MCP server's sign-in
}

// All are the services the UI pages and the agents' tools (package
// catalog) share, so both apply the same rules.
type All struct {
	Issues       *Issues
	Projects     *Projects
	Connections  *Connections
	Deliveries   *Deliveries
	Verification *Verification
	Logs         *Logs
	Agents       *Agents
	Users        *Users
}

func New(dependencies Dependencies) *All {
	settings, database := dependencies.Settings, dependencies.Store
	connections := NewConnections(dependencies.Logger, database, dependencies.Factory, dependencies.Registry, settings)

	return &All{
		Issues:       NewIssues(database),
		Projects:     NewProjects(database, dependencies.Registry, settings.Recording, settings.Notifications.PublicURL, settings.TrustedProxies),
		Connections:  connections,
		Deliveries:   NewDeliveries(database),
		Verification: NewVerification(dependencies.Logger, database, dependencies.Factory, connections),
		Logs:         NewLogs(database),
		Agents:       NewAgents(dependencies.OAuth, database),
		Users:        NewUsers(dependencies.Logger, database),
	}
}
