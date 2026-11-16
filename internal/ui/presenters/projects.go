package presenters

import (
	"strconv"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/helpers"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

// Projects presents projects: the table of the Projects page and the form.
type Projects struct {
	Retention config.Retention // the global settings
	Recording config.Recording // the global levels to record
	// FromEnv: the project comes from PROJECTS (only its retention and levels change).
	FromEnv func(projectID int) bool
	// Color is the color a project shows (services.Projects.Color).
	Color func(project store.Project) string
}

// Info is a row of the projects table.
func (presenter Projects) Info(project config.Project, dsn string, unresolvedIssues int) contracts.ProjectInfo {
	return contracts.ProjectInfo{
		ID: project.ID, Name: project.Name, DSN: dsn, UnresolvedIssues: unresolvedIssues,
		FromEnv: presenter.FromEnv(project.ID), Retention: helpers.RetentionDifferences(presenter.Retention, project.Retention),
		EventLevels: lists.InLevelOrder(presenter.Recording.EventLevelsFor(project.EventLevels), config.Levels),
		LogLevels:   presenter.Recording.LogLevelsFor(project.LogLevels),
	}
}

// Form fills the form from a project (empty for a new one): its own
// settings in the fields, the global ones as placeholders.
func (presenter Projects) Form(project store.Project) contracts.ProjectView {
	global := presenter.Retention

	return contracts.ProjectView{
		EventLevels: lists.LevelOptions(config.Levels, presenter.Recording.EventLevelsFor(project.EventLevels)),
		LogLevels:   lists.LevelOptions(config.LogLevels, presenter.Recording.LogLevelsFor(project.LogLevels)),
		New:         project.ID == 0, ID: project.ID, Name: project.Name, FromEnv: project.ID != 0 && presenter.FromEnv(project.ID),
		Color:  presenter.Color(project),
		Events: helpers.DurationField(project.RetentionEvents), Logs: helpers.DurationField(project.RetentionLogs),
		Issues: helpers.DurationField(project.RetentionIssues), EventsPerIssue: helpers.CountField(project.RetentionEventsPerIssue),
		LogsPerMessage: helpers.CountField(project.RetentionLogsPerMessage), DefaultLogsPerMessage: strconv.Itoa(global.LogsPerMessage),
		DefaultEvents: config.FormatDuration(global.Events), DefaultLogs: config.FormatDuration(global.Logs),
		DefaultIssues: config.FormatDuration(global.Issues), DefaultEventsPerIssue: strconv.Itoa(global.EventsPerIssue),
		Errors: map[string]string{},
	}
}

// Storage shows how much the database holds (used bytes) and what the
// global retention keeps.
func Storage(used int64, settings config.Retention) contracts.StorageInfo {
	info := contracts.StorageInfo{
		Used: config.FormatSize(used), Events: helpers.KeptFor(settings.Events), Logs: helpers.KeptFor(settings.Logs),
		EventsPerIssue: helpers.CountOrAll(settings.EventsPerIssue), Issues: helpers.KeptFor(settings.Issues),
		LogsPerMessage: helpers.CountOrAll(settings.LogsPerMessage),
	}

	if settings.MaxDatabaseBytes > 0 {
		info.Limit = config.FormatSize(settings.MaxDatabaseBytes)
		info.Percent = int(min(100, used*100/settings.MaxDatabaseBytes))
	}

	return info
}
