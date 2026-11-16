package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/devforth/exception-monitoring/internal/access"
	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/store"
)

// Projects answers what pages show about projects: names, DSNs, colors,
// unresolved issues and the storage used.
type Projects struct {
	store          *store.Store
	registry       *projects.Registry
	recording      config.Recording
	publicURL      string
	trustedProxies []netip.Prefix
}

func NewProjects(database *store.Store, registry *projects.Registry, recording config.Recording, publicURL string, trustedProxies []netip.Prefix) *Projects {
	return &Projects{store: database, registry: registry, recording: recording, publicURL: publicURL, trustedProxies: trustedProxies}
}

// FromEnv: the project comes from PROJECTS (its name and key are fixed).
func (service *Projects) FromEnv(id int) bool {
	return service.registry.FromEnv(id)
}

// Get reads a project with its settings; store.ErrNotFound when missing.
func (service *Projects) Get(ctx context.Context, id int) (store.Project, error) {
	return service.registry.Get(ctx, id)
}

// Save applies the form to project and stores it: a new one when its ID is
// 0, the stored one otherwise (a project from PROJECTS keeps its name).
// It returns the project as read from the form (with its id once saved) and
// the field errors when nothing was saved.
func (service *Projects) Save(ctx context.Context, form url.Values, project store.Project) (store.Project, map[string]string, error) {
	nameEditable := project.ID == 0 || !service.registry.FromEnv(project.ID)
	problems := readProjectForm(form, &project, nameEditable, service.recording, service.Color(store.Project{ID: project.ID}))

	if len(problems) > 0 {
		return project, problems, nil
	}

	var err error

	if project.ID == 0 {
		project.ID, err = service.registry.Create(ctx, project)
	} else {
		err = service.registry.Update(ctx, project)
	}

	if errors.Is(err, store.ErrProjectConflict) {
		return project, map[string]string{"name": "Another project already has this name."}, nil
	}

	return project, nil, err
}

// Delete removes a project added in the UI with its data; one from
// PROJECTS cannot be deleted (projects.ErrFromEnv).
func (service *Projects) Delete(ctx context.Context, id int) error {
	return service.registry.Delete(ctx, id)
}

// Names lists the current projects for the project filter.
func (service *Projects) Names() []string {
	list := service.registry.List()
	names := make([]string, 0, len(list))

	for _, project := range list {
		names = append(names, project.Name)
	}

	return names
}

// IDs gives the ids of the ?project= names; lists.CleanListQuery has kept
// only known projects, by name, each once. None means all projects.
func (service *Projects) IDs(names []string) []int {
	var ids []int

	for _, name := range names {
		if project, ok := service.registry.List().Find(name); ok {
			ids = append(ids, project.ID)
		}
	}

	return ids
}

// Name labels data of a project removed from PROJECTS by its ID.
func (service *Projects) Name(projectID int) string {
	if project, ok := service.registry.List().ByID(projectID); ok {
		return project.Name
	}

	return fmt.Sprintf("project %d", projectID)
}

// DSN is the DSN of a project at the server's address (see address).
func (service *Projects) DSN(r *http.Request, project config.Project) string {
	return project.DSN(service.address(r))
}

// URL is the address of path on this server, such as the MCP endpoint.
func (service *Projects) URL(r *http.Request, path string) string {
	return service.address(r).JoinPath(path).String()
}

// address is where clients reach the server (see access.PublicURL).
func (service *Projects) address(r *http.Request) *url.URL {
	return access.PublicURL(r, service.publicURL, service.trustedProxies)
}

// Color is the color a project shows: its own, or the palette color
// of its id (for a new project, of the id it will get).
func (service *Projects) Color(project store.Project) string {
	if project.Color != "" {
		return project.Color
	}

	id := project.ID

	if id == 0 {
		for _, existing := range service.registry.List() {
			id = max(id, existing.ID)
		}

		id++
	}

	return config.DefaultProjectColor(id)
}

// UnresolvedCounts gives the number of unresolved issues of each project, by id.
func (service *Projects) UnresolvedCounts(ctx context.Context) (map[int]int, error) {
	return service.store.UnresolvedCounts(ctx)
}

// DatabaseSize is how many bytes the database holds.
func (service *Projects) DatabaseSize(ctx context.Context) (int64, error) {
	return service.store.DatabaseSize(ctx)
}
