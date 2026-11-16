// Package projects keeps the list of projects: stored in the database,
// seeded from PROJECTS at startup, editable in the UI, and cached in memory
// so ingest reads it without a query per envelope.
//
// The projects in PROJECTS are written to the database at every start with
// the same id, name and key, so their DSNs never change and env stays the
// source of truth for them: the UI can change only their retention.
// Projects added in the UI are fully editable and can be deleted.
package projects

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
)

type Registry struct {
	store *store.Store
	env   config.Projects
	// current holds the list and its stylesheet together, so a reader never
	// sees one from before a change and the other from after.
	current atomic.Pointer[snapshot]
}

type snapshot struct {
	list   config.Projects
	colors Stylesheet
}

// Stylesheet sets the color of every project for the UI, which cannot use
// inline styles (CSP). Version changes with the content, for cache busting.
type Stylesheet struct {
	CSS     string
	Version string
}

var _ config.ProjectList = (*Registry)(nil)

// Open seeds the projects of PROJECTS and loads the list.
func Open(ctx context.Context, logger *slog.Logger, database *store.Store, env config.Projects) (*Registry, error) {
	registry := &Registry{store: database, env: env}

	for _, project := range env {
		if existing, err := database.GetProject(ctx, project.ID); err == nil && (existing.Name != project.Name || existing.PublicKey != project.PublicKey) {
			logger.Warn("project changed by PROJECTS", "id", project.ID, "name", project.Name, "previous_name", existing.Name,
				"key_changed", existing.PublicKey != project.PublicKey)
		}

		if err := database.SeedProject(ctx, store.Project{ID: project.ID, Name: project.Name, PublicKey: project.PublicKey}, time.Now()); err != nil {
			return nil, fmt.Errorf("PROJECTS: %w", err)
		}
	}

	if err := registry.Reload(ctx); err != nil {
		return nil, err
	}

	return registry, nil
}

// List returns the current projects.
func (registry *Registry) List() config.Projects {
	if current := registry.current.Load(); current != nil {
		return current.list
	}

	return nil
}

// FromEnv reports whether a project is defined in PROJECTS: its name and key
// are set there, not in the UI, and it cannot be deleted in the UI.
func (registry *Registry) FromEnv(id int) bool {
	_, ok := registry.env.ByID(id)

	return ok
}

// Reload reads the list from the database. Called after every change.
func (registry *Registry) Reload(ctx context.Context) error {
	stored, err := registry.store.ListProjects(ctx)

	if err != nil {
		return fmt.Errorf("load projects: %w", err)
	}

	list := make(config.Projects, 0, len(stored))

	for _, project := range stored {
		color := project.Color

		if color == "" {
			color = config.DefaultProjectColor(project.ID)
		}

		list = append(list, config.Project{
			ID: project.ID, Name: project.Name, PublicKey: project.PublicKey, Color: color,
			EventLevels: project.EventLevels, LogLevels: project.LogLevels,
			Retention: config.RetentionOverride{
				Events: project.RetentionEvents, Logs: project.RetentionLogs, Issues: project.RetentionIssues,
				EventsPerIssue: project.RetentionEventsPerIssue, LogsPerMessage: project.RetentionLogsPerMessage,
			},
		})
	}

	registry.current.Store(&snapshot{list: list, colors: stylesheet(list)})

	return nil
}

// Colors returns the stylesheet with the project colors.
func (registry *Registry) Colors() Stylesheet {
	if current := registry.current.Load(); current != nil {
		return current.colors
	}

	return Stylesheet{}
}

// stylesheet gives each project badge (data-project="<name>") its color.
// Names and colors are validated, so they are safe in CSS.
func stylesheet(list config.Projects) Stylesheet {
	var css strings.Builder

	for _, project := range list {
		fmt.Fprintf(&css, "[data-project=%q] { --project-color: %s; }\n", project.Name, project.Color)
	}

	sum := sha256.Sum256([]byte(css.String()))

	return Stylesheet{CSS: css.String(), Version: hex.EncodeToString(sum[:])[:12]}
}

// Get returns a project as stored, for editing.
func (registry *Registry) Get(ctx context.Context, id int) (store.Project, error) {
	return registry.store.GetProject(ctx, id)
}

// Create adds a project with a new DSN key; returns its id.
func (registry *Registry) Create(ctx context.Context, project store.Project) (int, error) {
	project.PublicKey = NewPublicKey()

	id, err := registry.store.CreateProject(ctx, project, time.Now())

	if err != nil {
		return 0, err
	}

	return id, registry.Reload(ctx)
}

// Update saves a project. For a project from PROJECTS only the retention
// changes: its name and key are kept as env has them.
func (registry *Registry) Update(ctx context.Context, project store.Project) error {
	if envProject, ok := registry.env.ByID(project.ID); ok {
		project.Name, project.PublicKey = envProject.Name, envProject.PublicKey
	}

	if err := registry.store.UpdateProject(ctx, project, time.Now()); err != nil {
		return err
	}

	return registry.Reload(ctx)
}

// ErrFromEnv: a project from PROJECTS cannot be deleted in the UI.
var ErrFromEnv = fmt.Errorf("the project is defined in PROJECTS; remove it there first")

// Delete removes a project and all its data.
func (registry *Registry) Delete(ctx context.Context, id int) error {
	if registry.FromEnv(id) {
		return ErrFromEnv
	}

	if err := registry.store.DeleteProject(ctx, id); err != nil {
		return err
	}

	return registry.Reload(ctx)
}

// NewPublicKey returns a random DSN key (32 hex characters).
func NewPublicKey() string {
	var key [16]byte

	_, _ = rand.Read(key[:])

	return hex.EncodeToString(key[:])
}
