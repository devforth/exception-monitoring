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
