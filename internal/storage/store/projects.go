package store

import (
	"errors"
	"time"
)

// Project is a row of the projects table. Nil retention fields use the
// global settings.
type Project struct {
	ID                      int
	Name                    string
	PublicKey               string
	RetentionEvents         *time.Duration
	RetentionLogs           *time.Duration
	RetentionIssues         *time.Duration
	RetentionEventsPerIssue *int
	RetentionLogsPerMessage *int
	// EventLevels and LogLevels: the levels recorded; nil uses the defaults.
	EventLevels *[]string
	LogLevels   *[]string
	Color       string // "#rrggbb", or "" for the palette color
	CreatedAt   time.Time
}

// ErrProjectConflict: the name or the key is already used by another project.
var ErrProjectConflict = errors.New("project name or key already used")
