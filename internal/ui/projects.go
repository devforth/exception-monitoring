package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/store"
)

// The Projects page: each project with its DSN and retention, the storage
// used, and adding, editing and deleting projects. Projects from PROJECTS
// keep the name and key set there; only their retention changes here.

type projectInfo struct {
	ID               int
	Name             string
	DSN              string
	UnresolvedIssues int
	FromEnv          bool
	Retention        string // what differs from the global settings, or empty
	EventLevels      []string
	LogLevels        []string // nil: every level
}

type projectsView struct {
	Projects []projectInfo
	Storage  storageInfo
	Notice   string
}

// storageInfo shows how much the database holds and what retention keeps.
type storageInfo struct {
	Used           string
	Limit          string // empty: no size limit
	Percent        int    // of the limit
	Events         string
	Logs           string
	EventsPerIssue string
	LogsPerMessage string
	Issues         string
}

var projectNotices = map[string]string{
	"created": "Project added. Put its DSN into the Sentry SDK config of the service.",
	"saved":   "Saved.",
	"deleted": "Project and its data deleted.",
}

// dsn is the DSN of a project at the address the admin opened the UI with;
// behind a proxy that is the public address.
func (handler *Handler) dsn(r *http.Request, project config.Project) string {
	scheme := "http"

	if handler.isHTTPS(r) {
		scheme = "https"
	}

	return fmt.Sprintf("%s://%s@%s/%d", scheme, project.PublicKey, r.Host, project.ID)
}

func (handler *Handler) projectsPage(w http.ResponseWriter, r *http.Request) {
	counts, err := handler.store.UnresolvedCounts(r.Context())

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	list := handler.registry.List()
	view := projectsView{Projects: make([]projectInfo, 0, len(list)), Notice: projectNotices[r.URL.Query().Get("notice")]}

	for _, project := range list {
		view.Projects = append(view.Projects, projectInfo{
			ID: project.ID, Name: project.Name, DSN: handler.dsn(r, project), UnresolvedIssues: counts[project.ID],
			FromEnv: handler.registry.FromEnv(project.ID), Retention: retentionDifferences(handler.retention, project.Retention),
			EventLevels: inLevelOrder(handler.recording.EventLevelsFor(project.EventLevels), config.Levels),
			LogLevels:   handler.recording.LogLevelsFor(project.LogLevels),
		})
	}

	if view.Storage, err = handler.storageInfo(r.Context()); err != nil {
		handler.serverError(w, r, err)

		return
	}

	handler.render(w, r, http.StatusOK, "projects", view)
}

// retentionDifferences lists a project's own retention, e.g. "logs 3 days".
func retentionDifferences(global config.Retention, override config.RetentionOverride) string {
	own := global.For(override)

	var parts []string

	for _, item := range []struct {
		label        string
		value, other time.Duration
	}{
		{"events", own.Events, global.Events},
		{"logs", own.Logs, global.Logs},
		{"issues", own.Issues, global.Issues},
	} {
		if item.value != item.other {
			parts = append(parts, item.label+" "+keptFor(item.value))
		}
	}

	if own.EventsPerIssue != global.EventsPerIssue {
		parts = append(parts, countOrAll(own.EventsPerIssue)+" events per issue")
	}

	if own.LogsPerMessage != global.LogsPerMessage {
		parts = append(parts, countOrAll(own.LogsPerMessage)+" logs per message")
	}

	return strings.Join(parts, ", ")
}

func (handler *Handler) storageInfo(ctx context.Context) (storageInfo, error) {
	used, err := handler.store.DatabaseSize(ctx)

	if err != nil {
		return storageInfo{}, err
	}

	settings := handler.retention
	info := storageInfo{
		Used: config.FormatSize(used), Events: keptFor(settings.Events), Logs: keptFor(settings.Logs),
		EventsPerIssue: countOrAll(settings.EventsPerIssue), Issues: keptFor(settings.Issues),
		LogsPerMessage: countOrAll(settings.LogsPerMessage),
	}

	if settings.MaxDatabaseBytes > 0 {
		info.Limit = config.FormatSize(settings.MaxDatabaseBytes)
		info.Percent = int(min(100, used*100/settings.MaxDatabaseBytes))
	}

	return info, nil
}

// countOrAll shows a count limit: "all" for 0 (no limit).
func countOrAll(count int) string {
	if count == 0 {
		return "all"
	}

	return strconv.Itoa(count)
}

func keptFor(duration time.Duration) string {
	switch days := int(duration.Hours() / 24); {
	case duration == 0:
		return "forever"
	case days == 1 && duration == 24*time.Hour:
		return "1 day"
	case days > 1 && duration%(24*time.Hour) == 0:
		return strconv.Itoa(days) + " days"
	default:
		return duration.String()
	}
}

// projectView is the form of a new or an existing project.
type projectView struct {
	New     bool
	ID      int
	Name    string
	DSN     string
	FromEnv bool
	Color   string // "#rrggbb" shown in the color input
	// Retention fields as typed: empty means the global value (shown as placeholder).
	Events, Logs, Issues, EventsPerIssue, LogsPerMessage                                    string
	DefaultEvents, DefaultLogs, DefaultIssues, DefaultEventsPerIssue, DefaultLogsPerMessage string
	// The levels recorded, as checkboxes (highest first).
	EventLevels, LogLevels []option
	Errors                 map[string]string
	Notice                 string
}

// levelOptions lists the known levels highest first, chosen when recorded
// (nil records every level).
func levelOptions(known, recorded []string) []option {
	levels := highestFirst(known)

	if recorded == nil {
		return options(levels, levels)
	}

	return options(levels, recorded)
}

// inLevelOrder orders levels highest first, as the known list does; nil
// (every level) stays nil.
func inLevelOrder(levels, known []string) []string {
	if levels == nil {
		return nil
	}

	return slices.DeleteFunc(highestFirst(known), func(level string) bool { return !slices.Contains(levels, level) })
}

func (handler *Handler) newProjectView(project store.Project) projectView {
	global := handler.retention

	return projectView{
		EventLevels: levelOptions(config.Levels, handler.recording.EventLevelsFor(project.EventLevels)),
		LogLevels:   levelOptions(config.LogLevels, handler.recording.LogLevelsFor(project.LogLevels)),
		New:         project.ID == 0, ID: project.ID, Name: project.Name, FromEnv: project.ID != 0 && handler.registry.FromEnv(project.ID),
		Color:  handler.projectColor(project),
		Events: durationField(project.RetentionEvents), Logs: durationField(project.RetentionLogs),
		Issues: durationField(project.RetentionIssues), EventsPerIssue: countField(project.RetentionEventsPerIssue),
		LogsPerMessage: countField(project.RetentionLogsPerMessage), DefaultLogsPerMessage: strconv.Itoa(global.LogsPerMessage),
		DefaultEvents: config.FormatDuration(global.Events), DefaultLogs: config.FormatDuration(global.Logs),
		DefaultIssues: config.FormatDuration(global.Issues), DefaultEventsPerIssue: strconv.Itoa(global.EventsPerIssue),
		Errors: map[string]string{},
	}
}

// formatDuration writes a duration the way the settings take it: "30d", "12h", "0".
// projectColor is the color a project shows: its own, or the palette color
// of its id (for a new project, of the id it will get).
func (handler *Handler) projectColor(project store.Project) string {
	if project.Color != "" {
		return project.Color
	}

	id := project.ID

	if id == 0 {
		for _, existing := range handler.registry.List() {
			id = max(id, existing.ID)
		}

		id++
	}

	return config.DefaultProjectColor(id)
}

func durationField(duration *time.Duration) string {
	if duration == nil {
		return ""
	}

	return config.FormatDuration(*duration)
}

func countField(count *int) string {
	if count == nil {
		return ""
	}

	return strconv.Itoa(*count)
}

// chosenLevels keeps the known levels of the form; nil when they are the
// defaults (nil defaults: every level).
func chosenLevels(values, known, defaults []string) *[]string {
	levels := inLevelOrder(values, known)

	if defaults == nil {
		defaults = known
	}

	if slices.Equal(levels, inLevelOrder(defaults, known)) {
		return nil
	}

	return &levels
}

func (handler *Handler) newProjectPage(w http.ResponseWriter, r *http.Request) {
	handler.render(w, r, http.StatusOK, "project", handler.newProjectView(store.Project{}))
}

// loadProject reads the project of the {projectID} path value; on failure it
// has written the response.
func (handler *Handler) loadProject(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	id, err := strconv.Atoi(r.PathValue("projectID"))

	if err != nil {
		http.NotFound(w, r)

		return store.Project{}, false
	}

	project, err := handler.registry.Get(r.Context(), id)

	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)

		return project, false
	case err != nil:
		handler.serverError(w, r, err)

		return project, false
	}

	return project, true
}

func (handler *Handler) projectPage(w http.ResponseWriter, r *http.Request) {
	project, ok := handler.loadProject(w, r)

	if !ok {
		return
	}

	view := handler.newProjectView(project)
	view.DSN = handler.dsn(r, config.Project{ID: project.ID, PublicKey: project.PublicKey})
	view.Notice = projectNotices[r.URL.Query().Get("notice")]

	handler.render(w, r, http.StatusOK, "project", view)
}

// readProjectForm applies the form to project and returns the field errors.
// The name is read only when it can change (not for a project from PROJECTS).
func (handler *Handler) readProjectForm(r *http.Request, project *store.Project, nameEditable bool) map[string]string {
	problems := map[string]string{}

	if nameEditable {
		project.Name = strings.TrimSpace(r.PostFormValue("name"))

		if !config.ValidProjectName(project.Name) {
			problems["name"] = "Use lowercase letters, digits, '-' and '_', starting with a letter or digit."
		}
	}

	// The palette color is stored as "" so the project keeps following the
	// palette; any other color is stored as chosen.
	if color := strings.ToLower(strings.TrimSpace(r.PostFormValue("color"))); color != "" {
		switch {
		case !config.ValidColor(color):
			problems["color"] = "Choose a color."
		case color == handler.projectColor(store.Project{ID: project.ID}):
			project.Color = ""
		default:
			project.Color = color
		}
	}

	for _, field := range []struct {
		name   string
		target **time.Duration
	}{
		{"retention_events", &project.RetentionEvents},
		{"retention_logs", &project.RetentionLogs},
		{"retention_issues", &project.RetentionIssues},
	} {
		value := strings.TrimSpace(r.PostFormValue(field.name))

		if value == "" {
			*field.target = nil

			continue
		}

		duration, err := config.ParseOptionalDuration(value)

		if err != nil {
			problems[field.name] = "Enter a period like 30d or 12h, 0 to keep forever, or leave empty for the default."

			continue
		}

		*field.target = &duration
	}

	// The levels to record. The whole list is sent with the form, so nothing
	// checked means record nothing; the defaults are stored as "use the
	// defaults", so a project follows RECORD_*_LEVELS until it differs.
	project.EventLevels = chosenLevels(r.PostForm["event_level"], config.Levels, handler.recording.EventLevels)
	project.LogLevels = chosenLevels(r.PostForm["log_level"], config.LogLevels, handler.recording.LogLevels)

	for _, field := range []struct {
		name   string
		target **int
	}{
		{"retention_events_per_issue", &project.RetentionEventsPerIssue},
		{"retention_logs_per_message", &project.RetentionLogsPerMessage},
	} {
		*field.target = nil
		value := strings.TrimSpace(r.PostFormValue(field.name))

		if value == "" {
			continue
		}

		count, err := config.ParseCount(value)

		if err != nil {
			problems[field.name] = "Enter a number, 0 to keep all, or leave empty for the default."

			continue
		}

		*field.target = &count
	}

	return problems
}

func (handler *Handler) renderProjectForm(w http.ResponseWriter, r *http.Request, project store.Project, problems map[string]string) {
	view := handler.newProjectView(project)
	view.Errors = problems

	// Show what was typed, not the parsed values.
	view.Events, view.Logs = r.PostFormValue("retention_events"), r.PostFormValue("retention_logs")
	view.Issues, view.EventsPerIssue = r.PostFormValue("retention_issues"), r.PostFormValue("retention_events_per_issue")
	view.LogsPerMessage = r.PostFormValue("retention_logs_per_message")

	if project.ID != 0 {
		view.DSN = handler.dsn(r, config.Project{ID: project.ID, PublicKey: project.PublicKey})
	}

	handler.render(w, r, http.StatusUnprocessableEntity, "project", view)
}

func (handler *Handler) saveError(w http.ResponseWriter, r *http.Request, project store.Project, err error) {
	if errors.Is(err, store.ErrProjectConflict) {
		handler.renderProjectForm(w, r, project, map[string]string{"name": "Another project already has this name."})

		return
	}

	handler.serverError(w, r, err)
}

func (handler *Handler) createProject(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	var project store.Project

	if problems := handler.readProjectForm(r, &project, true); len(problems) > 0 {
		handler.renderProjectForm(w, r, project, problems)

		return
	}

	id, err := handler.registry.Create(r.Context(), project)

	if err != nil {
		handler.saveError(w, r, project, err)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/projects/%d?notice=created", id))
}

func (handler *Handler) updateProject(w http.ResponseWriter, r *http.Request) {
	project, ok := handler.loadProject(w, r)

	if !ok {
		return
	}

	_ = r.ParseForm()

	if problems := handler.readProjectForm(r, &project, !handler.registry.FromEnv(project.ID)); len(problems) > 0 {
		handler.renderProjectForm(w, r, project, problems)

		return
	}

	if err := handler.registry.Update(r.Context(), project); err != nil {
		handler.saveError(w, r, project, err)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/projects/%d?notice=saved", project.ID))
}

func (handler *Handler) deleteProject(w http.ResponseWriter, r *http.Request) {
	project, ok := handler.loadProject(w, r)

	if !ok {
		return
	}

	if err := handler.registry.Delete(r.Context(), project.ID); err != nil {
		if errors.Is(err, projects.ErrFromEnv) {
			http.Error(w, err.Error(), http.StatusConflict)

			return
		}

		handler.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, "/projects?notice=deleted")
}
