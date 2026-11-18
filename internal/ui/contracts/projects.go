package contracts

import (
	"time"

	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

type ProjectInfo struct {
	ID               int
	Name             string
	DSN              string
	UnresolvedIssues int
	FromEnv          bool
	EventLevels      []string
	LogLevels        []string // nil: every level
}

type ProjectsView struct {
	Projects []ProjectInfo
	Storage  StorageInfo
	Notice   string
}

// StorageInfo shows how much the database holds and how it is cleaned.
type StorageInfo struct {
	Used        string
	Limit       string // empty: no size limit
	Percent     int    // of the limit
	CleanupStop int    // the cleanup stops at this percent of the limit
	Cleanups    []CleanupRunView
}

// CleanupRunView is a run of the cleanup by size, newest first on the Projects page.
type CleanupRunView struct {
	ID                                      uint64
	StartedAt                               time.Time
	Status                                  string // running, done or failed
	Limit, SizeBefore, Compacted, SizeAfter string
	Reached                                 int // the last step; 0: compacting was enough
	Duration                                string
	Error                                   string
	Steps                                   []CleanupStepView
}

// CleanupStepView is a step that deleted something, numbered as in docs/configuration.md.
type CleanupStepView struct {
	Number   int
	Name     string
	Freed    string
	Duration string
	Projects []CleanupProjectView
}

// CleanupProjectView is what a step deleted in one project: "120 logs, 3 events".
type CleanupProjectView struct {
	Project string
	Deleted string
}

// ProjectView is the form of a new or an existing project.
type ProjectView struct {
	New     bool
	ID      int
	Name    string
	DSN     string
	FromEnv bool
	Color   string // "#rrggbb" shown in the color input
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
