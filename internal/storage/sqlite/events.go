package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// timeFormat is fixed-width UTC with microseconds, so stored times sort as strings.
const timeFormat = "2006-01-02T15:04:05.000000Z"

func formatTime(value time.Time) string {
	return value.UTC().Format(timeFormat)
}

// Maximum stored lengths. Titles come from error messages and can be huge
// (e.g. a whole JSON document in the message).
const (
	maxTitleLength   = 500
	maxCulpritLength = 300
)

// SaveEvent stores the event and creates or updates its issue in one transaction.
//
// first_seen/last_seen use the server's receive time, not the SDK timestamp:
// browser clocks are often wrong, and the issue list must be ordered by
// when the server actually saw the problem.
func (store *Store) SaveEvent(ctx context.Context, event NewEvent) (SaveResult, error) {
	var result SaveResult

	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return result, err
	}

	defer transaction.Rollback()

	var existingEventID int64

	err = transaction.QueryRowContext(ctx,
		`SELECT id FROM events WHERE project_id = ? AND event_id = ?`,
		event.ProjectID, event.EventID).Scan(&existingEventID)

	if err == nil {
		result.Duplicate = true
		return result, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("check duplicate: %w", err)
	}

	title := truncate(event.Title, maxTitleLength)
	culprit := truncate(event.Culprit, maxCulpritLength)
	receivedAt := formatTime(event.ReceivedAt)

	var status string

	err = transaction.QueryRowContext(ctx,
		`SELECT id, status FROM issues WHERE project_id = ? AND fingerprint = ?`,
		event.ProjectID, event.Fingerprint).Scan(&result.IssueID, &status)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		result.NewIssue = true
		insert, err := transaction.ExecContext(ctx, `
			INSERT INTO issues (project_id, fingerprint, title, culprit, level, event_count, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
			event.ProjectID, event.Fingerprint, title, culprit, event.Level, receivedAt, receivedAt)

		if err != nil {
			return result, fmt.Errorf("create issue: %w", err)
		}

		if result.IssueID, err = insert.LastInsertId(); err != nil {
			return result, err
		}
	case err != nil:
		return result, fmt.Errorf("find issue: %w", err)
	default:
		result.Regression = status == StatusResolved
		newStatus := status

		if result.Regression {
			newStatus = StatusUnresolved
			// Recorded before the update, while the issue is still resolved.
			err := recordStatusChanges(ctx, transaction, "?", []any{result.IssueID}, StatusUnresolved, Actor{Source: SourceEvent}, event.EventID, event.ReceivedAt)

			if err != nil {
				return result, fmt.Errorf("record reopening: %w", err)
			}
		}

		// Title, culprit and level follow the latest event; ignored issues stay ignored.
		_, err := transaction.ExecContext(ctx, `
			UPDATE issues SET
				title = ?, culprit = ?, level = ?,
				event_count = event_count + 1,
				last_seen = max(last_seen, ?),
				status = ?,
				resolved_at = CASE WHEN ? = 'resolved' THEN resolved_at ELSE NULL END
			WHERE id = ?`,
			title, culprit, event.Level, receivedAt, newStatus, newStatus, result.IssueID)

		if err != nil {
			return result, fmt.Errorf("update issue: %w", err)
		}
	}

	_, err = transaction.ExecContext(ctx, `
		INSERT INTO events (project_id, issue_id, event_id, timestamp, received_at, level, environment, release, trace_id, span_id, parent_span_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ProjectID, result.IssueID, event.EventID, formatTime(event.Timestamp), receivedAt,
		event.Level, event.Environment, event.Release, event.TraceID, event.SpanID, event.ParentSpanID, string(event.Payload))

	if err != nil {
		return result, fmt.Errorf("insert event: %w", err)
	}

	if event.KeepEvents > 0 {
		// Through events_by_issue_time: only this issue's events are read.
		_, err = transaction.ExecContext(ctx, `
			DELETE FROM events WHERE id IN (
				SELECT id FROM events WHERE issue_id = ? ORDER BY received_at DESC LIMIT -1 OFFSET ?
			)`, result.IssueID, event.KeepEvents)

		if err != nil {
			return result, fmt.Errorf("trim issue events: %w", err)
		}
	}

	return result, transaction.Commit()
}

// SetIssueStatus changes an issue's status (resolve, ignore, reopen).
func (store *Store) SetIssueStatus(ctx context.Context, issueID int64, status string, actor Actor) error {
	_, err := store.SetIssuesStatus(ctx, []int64{issueID}, status, actor)

	return err
}

// SaveLogs stores a batch of log records in one transaction. With
// keepPerMessage > 0, each message of the batch then keeps only its newest
// keepPerMessage logs, so a line logged in a loop replaces its old copies
// instead of filling the database.
func (store *Store) SaveLogs(ctx context.Context, logs []NewLog, keepPerMessage int) error {
	if len(logs) == 0 {
		return nil
	}

	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	statement, err := transaction.PrepareContext(ctx, `
		INSERT INTO logs (project_id, timestamp, level, severity_number, body, trace_id, span_id, attributes, fingerprint)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)

	if err != nil {
		return err
	}

	defer statement.Close()

	for _, record := range logs {
		attributes := string(record.Attributes)

		if attributes == "" {
			attributes = "{}"
		}

		if _, err := statement.ExecContext(ctx, record.ProjectID, formatTime(record.Timestamp), record.Level,
			record.SeverityNumber, record.Body, record.TraceID, record.SpanID, attributes, record.Fingerprint); err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
	}

	if keepPerMessage > 0 {
		if err := trimMessages(ctx, transaction, logs, keepPerMessage); err != nil {
			return err
		}
	}

	return transaction.Commit()
}

// trimMessages keeps the newest keep logs of each message in the batch.
func trimMessages(ctx context.Context, transaction *sql.Tx, logs []NewLog, keep int) error {
	seen := map[logMessage]bool{}

	for _, record := range logs {
		message := logMessage{record.ProjectID, record.Fingerprint}

		if message.fingerprint == "" || seen[message] {
			continue
		}

		seen[message] = true

		if _, err := trimMessage(ctx, transaction, message, keep); err != nil {
			return err
		}
	}

	return nil
}

// logMessage is one repeated message: its project and fingerprint.
type logMessage struct {
	projectID   int
	fingerprint string
}

// trimMessage keeps the newest keep logs of one message. "fingerprint != ”"
// is repeated in both conditions: SQLite uses the partial index
// logs_by_fingerprint only when the query states its condition.
func trimMessage(ctx context.Context, db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, message logMessage, keep int,
) (int64, error) {
	result, err := db.ExecContext(ctx, `
		DELETE FROM logs WHERE project_id = ? AND fingerprint = ? AND fingerprint != '' AND id < (
			SELECT id FROM logs WHERE project_id = ? AND fingerprint = ? AND fingerprint != '' ORDER BY id DESC LIMIT 1 OFFSET ?
		)`, message.projectID, message.fingerprint, message.projectID, message.fingerprint, keep-1)

	if err != nil {
		return 0, fmt.Errorf("trim repeated logs: %w", err)
	}

	return result.RowsAffected()
}

// truncate cuts text to at most limit characters (not bytes), adding "…".
func truncate(text string, limit int) string {
	runes := []rune(text)

	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit-1]) + "…"
}
