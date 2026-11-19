package store

import (
	"time"
)

// Where a status change came from.
const (
	SourceUI    = "ui"
	SourceMCP   = "mcp"
	SourceAPI   = "api"
	SourceEvent = "event" // a resolved issue happened again
)

// Actor is who changes a status: a user (UserID empty for the API token, an
// event, or a deleted user, whose Name stays) and through what.
type Actor struct {
	UserID string
	Name   string
	Source string
}

// StatusChange is one entry of an issue's status history.
type StatusChange struct {
	Actor
	From, To  string
	EventID   string // the event that reopened the issue (SourceEvent)
	CreatedAt time.Time
}

// ByEvent: the issue was reopened because it happened again.
func (change StatusChange) ByEvent() bool { return change.Source == SourceEvent }

// ViaMCP: a coding agent changed it for its user.
func (change StatusChange) ViaMCP() bool { return change.Source == SourceMCP }
