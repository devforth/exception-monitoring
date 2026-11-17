// Package services holds the rules of the application and is the only way
// the UI pages and the agents' tools (package catalog) reach the database:
// issues, their events and actions, logs, projects, notification connections
// (duplicates, default names, saving), the delivery log, email verification,
// users and their passwords, and connected agents. It knows nothing about
// pages: it returns data and the results in results.go, and a change of
// several steps is one method, so a caller cannot do half of it.
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
	Store    store.Store
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
