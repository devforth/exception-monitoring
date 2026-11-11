package store

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

// Issue statuses.
const (
	StatusUnresolved = "unresolved"
	StatusResolved   = "resolved"
	StatusIgnored    = "ignored"
)

// NewEvent is an event ready to store: already parsed and grouped.
// The store does not know the Sentry format; the caller fills this in.
type NewEvent struct {
	ProjectID   int
	EventID     string
	Fingerprint string
	Title       string
	Culprit     string
	Level       string
	Environment string
	Release     string
	TraceID     string
	Timestamp   time.Time // when it happened, by the SDK's clock
	ReceivedAt  time.Time // when the server got it
	Payload     []byte    // full event JSON
}

// SaveResult tells what storing an event changed. Notifications (stage 6) use it.
type SaveResult struct {
	IssueID int64
	// Duplicate: an event with this event_id was already stored (an SDK retry); nothing changed.
	Duplicate bool
	// NewIssue: the first event of a new issue.
	NewIssue bool
	// Regression: the issue was resolved and happened again, so it is unresolved now.
	Regression bool
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
		INSERT INTO events (project_id, issue_id, event_id, timestamp, received_at, level, environment, release, trace_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ProjectID, result.IssueID, event.EventID, formatTime(event.Timestamp), receivedAt,
		event.Level, event.Environment, event.Release, event.TraceID, string(event.Payload))

	if err != nil {
		return result, fmt.Errorf("insert event: %w", err)
	}

	return result, transaction.Commit()
}

// SetIssueStatus changes an issue's status (resolve, ignore, reopen).
func (store *Store) SetIssueStatus(ctx context.Context, issueID int64, status string) error {
	switch status {
	case StatusUnresolved, StatusResolved, StatusIgnored:
	default:
		return fmt.Errorf("invalid status %q", status)
	}

	var resolvedAt any

	if status == StatusResolved {
		resolvedAt = formatTime(time.Now())
	}

	update, err := store.DB.ExecContext(ctx,
		`UPDATE issues SET status = ?, resolved_at = ? WHERE id = ?`, status, resolvedAt, issueID)

	if err != nil {
		return err
	}

	if count, err := update.RowsAffected(); err == nil && count == 0 {
		return ErrNotFound
	}

	return nil
}

// NewLog is a log record ready to store.
type NewLog struct {
	ProjectID      int
	Timestamp      time.Time
	Level          string
	SeverityNumber int
	Body           string
	TraceID        string
	Attributes     []byte // JSON object
}

// SaveLogs stores a batch of log records in one transaction.
func (store *Store) SaveLogs(ctx context.Context, logs []NewLog) error {
	if len(logs) == 0 {
		return nil
	}

	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	statement, err := transaction.PrepareContext(ctx, `
		INSERT INTO logs (project_id, timestamp, level, severity_number, body, trace_id, attributes)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)

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
			record.SeverityNumber, record.Body, record.TraceID, attributes); err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
	}

	return transaction.Commit()
}

// truncate cuts text to at most limit characters (not bytes), adding "…".
func truncate(text string, limit int) string {
	runes := []rune(text)

	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit-1]) + "…"
}
