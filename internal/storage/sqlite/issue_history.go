package sqlite

import (
	"context"
	"database/sql"
	"time"
)

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
