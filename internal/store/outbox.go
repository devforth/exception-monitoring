package store

import (
	"context"
	"fmt"
	"time"
)

// Outbox statuses of a notification message.
const (
	OutboxPending = "pending"
	OutboxSent    = "sent"
	OutboxFailed  = "failed"
)

// OutboxMessage is one message waiting to be sent to one channel. The store
// keeps the payloads opaque; package notify encodes and decodes them.
type OutboxMessage struct {
	ID           int64
	Channel      string
	Kind         string // "notification" or "summary"
	Payload      []byte
	EventPayload []byte // the full event JSON for formats with details; may be empty
	Attempts     int
	CreatedAt    time.Time
}

// EnqueueOutbox adds the message once per channel, due immediately.
func (store *Store) EnqueueOutbox(ctx context.Context, channels []string, kind string, payload, eventPayload []byte, now time.Time) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	statement, err := transaction.PrepareContext(ctx, `
		INSERT INTO notification_outbox (channel, kind, payload, event_payload, created_at, next_attempt_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)

	if err != nil {
		return err
	}

	defer statement.Close()

	stamp := formatTime(now)

	for _, channel := range channels {
		if _, err := statement.ExecContext(ctx, channel, kind, string(payload), string(eventPayload), stamp, stamp, stamp); err != nil {
			return fmt.Errorf("enqueue notification: %w", err)
		}
	}

	return transaction.Commit()
}

// DueOutbox returns pending messages of a channel whose next attempt is due,
// oldest first, so a channel sends in the order notifications happened.
func (store *Store) DueOutbox(ctx context.Context, channel string, now time.Time, limit int) ([]OutboxMessage, error) {
	rows, err := store.DB.QueryContext(ctx, `
		SELECT id, channel, kind, payload, event_payload, attempts, created_at
		FROM notification_outbox
		WHERE channel = ? AND status = 'pending' AND next_attempt_at <= ?
		ORDER BY id LIMIT ?`, channel, formatTime(now), limit)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var messages []OutboxMessage

	for rows.Next() {
		var (
			message               OutboxMessage
			payload, eventPayload string
			createdAt             string
		)

		if err := rows.Scan(&message.ID, &message.Channel, &message.Kind, &payload, &eventPayload, &message.Attempts, &createdAt); err != nil {
			return nil, err
		}

		message.Payload, message.EventPayload, message.CreatedAt = []byte(payload), []byte(eventPayload), parseTime(createdAt)
		messages = append(messages, message)
	}

	return messages, rows.Err()
}

func (store *Store) MarkOutboxSent(ctx context.Context, id int64, now time.Time) error {
	_, err := store.DB.ExecContext(ctx,
		`UPDATE notification_outbox SET status = 'sent', attempts = attempts + 1, last_error = '', updated_at = ? WHERE id = ?`,
		formatTime(now), id)

	return err
}

// MarkOutboxRetry records a failed attempt and when to try again.
func (store *Store) MarkOutboxRetry(ctx context.Context, id int64, lastError string, next, now time.Time) error {
	_, err := store.DB.ExecContext(ctx,
		`UPDATE notification_outbox SET attempts = attempts + 1, last_error = ?, next_attempt_at = ?, updated_at = ? WHERE id = ?`,
		lastError, formatTime(next), formatTime(now), id)

	return err
}

// MarkOutboxFailed gives up on a message after its last attempt.
func (store *Store) MarkOutboxFailed(ctx context.Context, id int64, lastError string, now time.Time) error {
	_, err := store.DB.ExecContext(ctx,
		`UPDATE notification_outbox SET status = 'failed', attempts = attempts + 1, last_error = ?, updated_at = ? WHERE id = ?`,
		lastError, formatTime(now), id)

	return err
}

// CleanOutbox deletes sent messages finished before sentBefore and failed ones
// before failedBefore, so the table does not grow forever.
func (store *Store) CleanOutbox(ctx context.Context, sentBefore, failedBefore time.Time) (int64, error) {
	result, err := store.DB.ExecContext(ctx, `
		DELETE FROM notification_outbox
		WHERE (status = 'sent' AND updated_at < ?) OR (status = 'failed' AND updated_at < ?)`,
		formatTime(sentBefore), formatTime(failedBefore))

	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// OutboxCounts returns the number of messages per status, for tests and diagnostics.
func (store *Store) OutboxCounts(ctx context.Context) (map[string]int, error) {
	rows, err := store.DB.QueryContext(ctx, `SELECT status, COUNT(*) FROM notification_outbox GROUP BY status`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	counts := map[string]int{}

	for rows.Next() {
		var (
			status string
			count  int
		)

		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}

		counts[status] = count
	}

	return counts, rows.Err()
}

// PendingChannels lists channels with messages still to send, so their
// workers start after a restart.
func (store *Store) PendingChannels(ctx context.Context) ([]string, error) {
	rows, err := store.DB.QueryContext(ctx, `SELECT DISTINCT channel FROM notification_outbox WHERE status = 'pending' ORDER BY channel`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var channels []string

	for rows.Next() {
		var channel string

		if err := rows.Scan(&channel); err != nil {
			return nil, err
		}

		channels = append(channels, channel)
	}

	return channels, rows.Err()
}
