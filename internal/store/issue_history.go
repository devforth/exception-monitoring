package store

import (
	"context"
	"database/sql"
	"time"
)

// Where a status change came from.
const (
	SourceUI    = "ui"
	SourceMCP   = "mcp"
	SourceAPI   = "api"
	SourceEvent = "event" // a resolved issue happened again
)

// Actor is who changes a status: a user (UserID 0 for the API token, an
// event, or a deleted user, whose Name stays) and through what.
type Actor struct {
	UserID int64
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

// IssueStatusHistory lists the status changes of an issue, newest first.
func (store *Store) IssueStatusHistory(ctx context.Context, issueID int64) ([]StatusChange, error) {
	rows, err := store.DB.QueryContext(ctx, `SELECT from_status, to_status, user_id, user_name, source, event_id, created_at
		FROM issue_status_changes WHERE issue_id = ? ORDER BY id DESC`, issueID)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var changes []StatusChange

	for rows.Next() {
		var (
			change    StatusChange
			userID    sql.NullInt64
			createdAt string
		)

		if err := rows.Scan(&change.From, &change.To, &userID, &change.Name, &change.Source, &change.EventID, &createdAt); err != nil {
			return nil, err
		}

		change.UserID, change.CreatedAt = userID.Int64, parseTime(createdAt)
		changes = append(changes, change)
	}

	return changes, rows.Err()
}

// recordStatusChanges adds an entry to the history of every listed issue
// whose status is not already status, in the transaction that changes it:
// one statement, before the update.
func recordStatusChanges(ctx context.Context, transaction *sql.Tx, placeholders string, ids []any, status string, actor Actor, eventID string, now time.Time) error {
	var userID any

	if actor.UserID > 0 {
		userID = actor.UserID
	}

	args := append([]any{status, userID, actor.Name, actor.Source, eventID, formatTime(now)}, ids...)
	args = append(args, status)
	_, err := transaction.ExecContext(ctx, `INSERT INTO issue_status_changes
		(issue_id, from_status, to_status, user_id, user_name, source, event_id, created_at)
		SELECT id, status, ?, ?, ?, ?, ?, ? FROM issues WHERE id IN (`+placeholders+`) AND status != ?`, args...)

	return err
}
