// Package requests reads and validates the forms the UI posts: projects,
// notification connections and their chat secrets, bulk issue actions. Each
// reader applies the form to a value and returns field errors (or a
// problem) for the controller to show; dependencies come as parameters.
package requests

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

// ReadProjectForm applies the form to project and returns the field errors.
// The name is read only when it can change (not for a project from PROJECTS).
// paletteColor is the color the project has without its own one.
func ReadProjectForm(r *http.Request, project *store.Project, nameEditable bool, recording config.Recording, paletteColor string) map[string]string {
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
		case color == paletteColor:
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
	project.EventLevels = chosenLevels(r.PostForm["event_level"], config.Levels, recording.EventLevels)
	project.LogLevels = chosenLevels(r.PostForm["log_level"], config.LogLevels, recording.LogLevels)

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

// chosenLevels keeps the known levels of the form; nil when they are the
// defaults (nil defaults: every level).
func chosenLevels(values, known, defaults []string) *[]string {
	levels := lists.InLevelOrder(values, known)

	if defaults == nil {
		defaults = known
	}

	if slices.Equal(levels, lists.InLevelOrder(defaults, known)) {
		return nil
	}

	return &levels
}
