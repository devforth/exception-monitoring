package contracts

import "github.com/devforth/exception-monitoring/internal/ui/lists"

type ProjectInfo struct {
	ID               int
	Name             string
	DSN              string
	UnresolvedIssues int
	FromEnv          bool
	Retention        string // what differs from the global settings, or empty
	EventLevels      []string
	LogLevels        []string // nil: every level
}

type ProjectsView struct {
	Projects []ProjectInfo
	Storage  StorageInfo
	Notice   string
}

// StorageInfo shows how much the database holds and what retention keeps.
type StorageInfo struct {
	Used           string
	Limit          string // empty: no size limit
	Percent        int    // of the limit
	Events         string
	Logs           string
	EventsPerIssue string
	LogsPerMessage string
	Issues         string
}

// ProjectView is the form of a new or an existing project.
type ProjectView struct {
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
	EventLevels, LogLevels []lists.Option
	Errors                 map[string]string
	Notice                 string
}

// RecordedEventLevels and RecordedLogLevels are the checked levels, shown
// while the Levels to record section is closed.
func (view ProjectView) RecordedEventLevels() []string { return checked(view.EventLevels) }

func (view ProjectView) RecordedLogLevels() []string { return checked(view.LogLevels) }

func checked(options []lists.Option) []string {
	var names []string

	for _, option := range options {
		if option.Selected {
			names = append(names, option.Name)
		}
	}

	return names
}

// RetentionSetting is one retention field as the closed Retention section
// shows it: the typed value, or the default.
type RetentionSetting struct {
	Field   string // the form field (app.js updates the summary from it)
	Label   string
	Value   string
	Zero    string // what 0 means: "forever" or "all"
	Default bool   // the field is empty: the global value applies
}

// RetentionSummary lists the retention fields with what applies to each.
func (view ProjectView) RetentionSummary() []RetentionSetting {
	setting := func(field, label, value, fallback, zero string) RetentionSetting {
		result := RetentionSetting{Field: field, Label: label, Value: value, Zero: zero}

		if value == "" {
			result.Value, result.Default = fallback, true
		}

		if result.Value == "0" {
			result.Value = zero
		}

		return result
	}

	return []RetentionSetting{
		setting("retention_events", "events", view.Events, view.DefaultEvents, "forever"),
		setting("retention_logs", "logs", view.Logs, view.DefaultLogs, "forever"),
		setting("retention_logs_per_message", "logs per message", view.LogsPerMessage, view.DefaultLogsPerMessage, "all"),
		setting("retention_events_per_issue", "events per issue", view.EventsPerIssue, view.DefaultEventsPerIssue, "all"),
		setting("retention_issues", "issues not seen", view.Issues, view.DefaultIssues, "forever"),
	}
}

// RetentionInvalid: a retention field was refused, so its section opens.
func (view ProjectView) RetentionInvalid() bool {
	for _, field := range []string{"retention_events", "retention_logs", "retention_logs_per_message", "retention_events_per_issue", "retention_issues"} {
		if view.Errors[field] != "" {
			return true
		}
	}

	return false
}
