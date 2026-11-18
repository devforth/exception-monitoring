package controllers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/projects"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/presenters"
)

// The Projects page: each project with its DSN and recorded levels, the
// storage used, and adding, editing and deleting projects. Projects from
// PROJECTS keep the name and key set there.

var projectNotices = map[string]string{
	"created": "Project added. Put its DSN into the Sentry SDK config of the service.",
	"saved":   "Saved.",
	"deleted": "Project and its data deleted.",
}

func (controller *Controllers) ProjectsPage(w http.ResponseWriter, r *http.Request) {
	counts, err := controller.projects.UnresolvedCounts(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	used, err := controller.projects.DatabaseSize(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	runs, err := controller.projects.Cleanups(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	list := controller.registry.List()
	view := contracts.ProjectsView{
		Projects: make([]contracts.ProjectInfo, 0, len(list)), Notice: projectNotices[r.URL.Query().Get("notice")],
		Storage: presenters.Storage(used, controller.retention),
	}

	view.Storage.Cleanups = presenters.CleanupRuns(runs, func(id int) string {
		if project, ok := config.Projects(list).ByID(id); ok {
			return project.Name
		}

		return fmt.Sprintf("#%d", id)
	})

	for _, project := range list {
		view.Projects = append(view.Projects, controller.projectViews.Info(project, controller.projects.DSN(r, project), counts[project.ID]))
	}

	controller.views.Render(w, r, http.StatusOK, "projects", view)
}

func (controller *Controllers) NewProjectPage(w http.ResponseWriter, r *http.Request) {
	controller.views.Render(w, r, http.StatusOK, "project", controller.projectViews.Form(store.Project{}))
}

// loadProject reads the project of the {projectID} path value; on failure it
// has written the response.
func (controller *Controllers) loadProject(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	id, ok := controller.pathID(w, r, "projectID")

	if !ok {
		return store.Project{}, false
	}

	project, err := controller.projects.Get(r.Context(), int(id))

	return project, !controller.failed(w, r, err)
}

func (controller *Controllers) ProjectPage(w http.ResponseWriter, r *http.Request) {
	project, ok := controller.loadProject(w, r)

	if !ok {
		return
	}

	view := controller.projectViews.Form(project)
	view.DSN = controller.projects.DSN(r, config.Project{ID: project.ID, PublicKey: project.PublicKey})
	view.Notice = projectNotices[r.URL.Query().Get("notice")]

	controller.views.Render(w, r, http.StatusOK, "project", view)
}

func (controller *Controllers) renderProjectForm(w http.ResponseWriter, r *http.Request, project store.Project, problems map[string]string) {
	view := controller.projectViews.Form(project)
	view.Errors = problems

	if project.ID != 0 {
		view.DSN = controller.projects.DSN(r, config.Project{ID: project.ID, PublicKey: project.PublicKey})
	}

	controller.views.Render(w, r, http.StatusUnprocessableEntity, "project", view)
}

func (controller *Controllers) CreateProject(w http.ResponseWriter, r *http.Request) {
	controller.saveProject(w, r, store.Project{}, "created")
}

func (controller *Controllers) UpdateProject(w http.ResponseWriter, r *http.Request) {
	project, ok := controller.loadProject(w, r)

	if ok {
		controller.saveProject(w, r, project, "saved")
	}
}

// saveProject stores the posted form (services.Projects.Save) or shows it
// again with the problems.
func (controller *Controllers) saveProject(w http.ResponseWriter, r *http.Request, project store.Project, notice string) {
	_ = r.ParseForm()

	saved, problems, err := controller.projects.Save(r.Context(), r.PostForm, project)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if len(problems) > 0 {
		controller.renderProjectForm(w, r, saved, problems)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/projects/%d?notice=%s", saved.ID, notice))
}

func (controller *Controllers) DeleteProject(w http.ResponseWriter, r *http.Request) {
	project, ok := controller.loadProject(w, r)

	if !ok {
		return
	}

	if err := controller.projects.Delete(r.Context(), project.ID); err != nil {
		if errors.Is(err, projects.ErrFromEnv) {
			http.Error(w, err.Error(), http.StatusConflict)

			return
		}

		controller.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, "/projects?notice=deleted")
}
