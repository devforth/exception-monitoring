package services

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"

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
	publicURL      string
	trustedProxies []netip.Prefix
}

func NewProjects(database *store.Store, registry *projects.Registry, publicURL string, trustedProxies []netip.Prefix) *Projects {
	return &Projects{store: database, registry: registry, publicURL: publicURL, trustedProxies: trustedProxies}
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

// DSN is the DSN of a project, at the server's address (see address). The
// scheme stays: SDKs need it to parse the DSN.
func (service *Projects) DSN(r *http.Request, project config.Project) string {
	base := service.address(r)
	base.User = url.User(project.PublicKey)

	return base.JoinPath(strconv.Itoa(project.ID)).String()
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
