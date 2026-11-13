package store

import (
	"context"
	"fmt"
	"strings"
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

// RetryOutboxNow makes the waiting messages of a channel due now: after its
// connection is fixed (a token, a scope), they need not wait for the next
// step of the retry schedule. The channel's worker picks them up on its next
// poll.
func (store *Store) RetryOutboxNow(ctx context.Context, channel string, now time.Time) error {
	_, err := store.DB.ExecContext(ctx,
		`UPDATE notification_outbox SET next_attempt_at = ? WHERE channel = ? AND status = 'pending' AND next_attempt_at > ?`,
		formatTime(now), channel, formatTime(now))

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

// OutboxRecord is a row of the outbox as the delivery log shows it.
type OutboxRecord struct {
	ID            int64
	Channel       string // the connection id
	Kind          string // "notification" or "summary"
	Payload       []byte
	Status        string
	Attempts      int
	LastError     string
	CreatedAt     time.Time
	NextAttemptAt time.Time
	UpdatedAt     time.Time
}

// OutboxFilter selects outbox rows, newest first. Zero values mean "any".
type OutboxFilter struct {
	Channel string
	Status  string
	Limit   int
	Offset  int
}

func (filter OutboxFilter) where() (string, []any) {
	var (
		where []string
		args  []any
	)

	if filter.Channel != "" {
		where, args = append(where, "channel = ?"), append(args, filter.Channel)
	}

	if filter.Status != "" {
		where, args = append(where, "status = ?"), append(args, filter.Status)
	}

	if len(where) == 0 {
		return "", nil
	}

	return " WHERE " + strings.Join(where, " AND "), args
}

// ListOutbox returns outbox rows for the delivery log, newest first.
func (store *Store) ListOutbox(ctx context.Context, filter OutboxFilter) ([]OutboxRecord, error) {
	where, args := filter.where()
	rows, err := store.DB.QueryContext(ctx, `
		SELECT id, channel, kind, payload, status, attempts, last_error, created_at, next_attempt_at, updated_at
		FROM notification_outbox`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, filter.Limit, filter.Offset)...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var records []OutboxRecord

	for rows.Next() {
		var (
			record                                   OutboxRecord
			payload, createdAt, nextAttempt, updated string
		)

		if err := rows.Scan(&record.ID, &record.Channel, &record.Kind, &payload, &record.Status, &record.Attempts,
			&record.LastError, &createdAt, &nextAttempt, &updated); err != nil {
			return nil, err
		}

		record.Payload = []byte(payload)
		record.CreatedAt, record.NextAttemptAt, record.UpdatedAt = parseTime(createdAt), parseTime(nextAttempt), parseTime(updated)
		records = append(records, record)
	}

	return records, rows.Err()
}

// CountOutbox counts the rows ListOutbox would page through.
func (store *Store) CountOutbox(ctx context.Context, filter OutboxFilter) (int, error) {
	where, args := filter.where()

	var count int

	err := store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_outbox`+where, args...).Scan(&count)

	return count, err
}

// DeliveryHealth sums up the delivery of one connection: whether its last
// attempts fail, for the notifications list.
type DeliveryHealth struct {
	Pending     int
	Retrying    int // pending after a failed attempt
	Failed      int // given up
	LastSentAt  time.Time
	LastError   string
	LastErrorAt time.Time
}

// Failing: messages are waiting for a retry, or the latest outcome was a failure.
func (health DeliveryHealth) Failing() bool {
	return health.Retrying > 0 || (!health.LastErrorAt.IsZero() && health.LastErrorAt.After(health.LastSentAt))
}

// DeliveryHealthByChannel returns the delivery health of every channel with
// rows in the outbox. A sent row has no error (MarkOutboxSent clears it), so
// the latest error belongs to a retrying or failed message.
func (store *Store) DeliveryHealthByChannel(ctx context.Context) (map[string]DeliveryHealth, error) {
	rows, err := store.DB.QueryContext(ctx, `
		SELECT channel,
			SUM(status = 'pending'), SUM(status = 'pending' AND attempts > 0), SUM(status = 'failed'),
			COALESCE(MAX(CASE WHEN status = 'sent' THEN updated_at END), ''),
			COALESCE((SELECT last_error FROM notification_outbox AS latest
				WHERE latest.channel = outbox.channel AND latest.last_error != '' ORDER BY latest.updated_at DESC LIMIT 1), ''),
			COALESCE(MAX(CASE WHEN last_error != '' THEN updated_at END), '')
		FROM notification_outbox AS outbox GROUP BY channel`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	health := map[string]DeliveryHealth{}

	for rows.Next() {
		var (
			channel, lastSent, lastErrorAt string
			channelHealth                  DeliveryHealth
		)

		if err := rows.Scan(&channel, &channelHealth.Pending, &channelHealth.Retrying, &channelHealth.Failed,
			&lastSent, &channelHealth.LastError, &lastErrorAt); err != nil {
			return nil, err
		}

		channelHealth.LastSentAt, channelHealth.LastErrorAt = parseTime(lastSent), parseTime(lastErrorAt)
		health[channel] = channelHealth
	}

	return health, rows.Err()
}
