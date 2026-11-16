package controllers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/presenters"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// The Projects page: each project with its DSN and retention, the storage
// used, and adding, editing and deleting projects. Projects from PROJECTS
// keep the name and key set there; only their retention changes here.

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

	list := controller.registry.List()
	view := contracts.ProjectsView{
		Projects: make([]contracts.ProjectInfo, 0, len(list)), Notice: projectNotices[r.URL.Query().Get("notice")],
		Storage: presenters.Storage(used, controller.retention),
	}

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

	project, err := controller.registry.Get(r.Context(), int(id))

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

// readProjectForm applies the form to project (see requests.ReadProjectForm).
func (controller *Controllers) readProjectForm(r *http.Request, project *store.Project, nameEditable bool) map[string]string {
	paletteColor := controller.projects.Color(store.Project{ID: project.ID})

	return requests.ReadProjectForm(r, project, nameEditable, controller.recording, paletteColor)
}

func (controller *Controllers) renderProjectForm(w http.ResponseWriter, r *http.Request, project store.Project, problems map[string]string) {
	view := controller.projectViews.Form(project)
	view.Errors = problems

	// Show what was typed, not the parsed values.
	view.Events, view.Logs = r.PostFormValue("retention_events"), r.PostFormValue("retention_logs")
	view.Issues, view.EventsPerIssue = r.PostFormValue("retention_issues"), r.PostFormValue("retention_events_per_issue")
	view.LogsPerMessage = r.PostFormValue("retention_logs_per_message")

	if project.ID != 0 {
		view.DSN = controller.projects.DSN(r, config.Project{ID: project.ID, PublicKey: project.PublicKey})
	}

	controller.views.Render(w, r, http.StatusUnprocessableEntity, "project", view)
}

func (controller *Controllers) saveError(w http.ResponseWriter, r *http.Request, project store.Project, err error) {
	if errors.Is(err, store.ErrProjectConflict) {
		controller.renderProjectForm(w, r, project, map[string]string{"name": "Another project already has this name."})

		return
	}

	controller.serverError(w, r, err)
}

func (controller *Controllers) CreateProject(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	var project store.Project

	if problems := controller.readProjectForm(r, &project, true); len(problems) > 0 {
		controller.renderProjectForm(w, r, project, problems)

		return
	}

	id, err := controller.registry.Create(r.Context(), project)

	if err != nil {
		controller.saveError(w, r, project, err)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/projects/%d?notice=created", id))
}

func (controller *Controllers) UpdateProject(w http.ResponseWriter, r *http.Request) {
	project, ok := controller.loadProject(w, r)

	if !ok {
		return
	}

	_ = r.ParseForm()

	if problems := controller.readProjectForm(r, &project, !controller.registry.FromEnv(project.ID)); len(problems) > 0 {
		controller.renderProjectForm(w, r, project, problems)

		return
	}

	if err := controller.registry.Update(r.Context(), project); err != nil {
		controller.saveError(w, r, project, err)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/projects/%d?notice=saved", project.ID))
}

func (controller *Controllers) DeleteProject(w http.ResponseWriter, r *http.Request) {
	project, ok := controller.loadProject(w, r)

	if !ok {
		return
	}

	if err := controller.registry.Delete(r.Context(), project.ID); err != nil {
		if errors.Is(err, projects.ErrFromEnv) {
			http.Error(w, err.Error(), http.StatusConflict)

			return
		}

		controller.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, "/projects?notice=deleted")
}
