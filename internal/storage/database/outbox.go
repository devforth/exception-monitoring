package database

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// outboxRecord is a notification message for one channel and what happened to it.
type outboxRecord struct {
	model.OutboxMessage
	Status        string    `json:"status"`
	LastError     string    `json:"last_error,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// EnqueueOutbox adds the message once per channel, due immediately.
func (database *Store) EnqueueOutbox(_ context.Context, channels []string, kind string, payload, eventPayload []byte, now time.Time) error {
	ids := make([]int64, len(channels))

	for index := range channels {
		sequence, err := database.db.Next()

		if err != nil {
			return err
		}

		ids[index] = int64(sequence)
	}

	return database.db.Update(true, func(batch *kv.Batch) error {
		for index, channel := range channels {
			message := outboxRecord{
				OutboxMessage: model.OutboxMessage{ID: ids[index], Channel: channel, Kind: kind, Payload: payload, EventPayload: eventPayload, CreatedAt: now.UTC()},
				Status:        model.OutboxPending, NextAttemptAt: now.UTC(), UpdatedAt: now.UTC(),
			}

			if err := database.outbox.Insert(batch, message); err != nil {
				return err
			}
		}

		return nil
	})
}

// DueOutbox returns the pending messages of a channel whose next attempt is
// due, oldest first, so a channel sends in the order notifications happened.
func (database *Store) DueOutbox(_ context.Context, channel string, now time.Time, limit int) ([]model.OutboxMessage, error) {
	var messages []model.OutboxMessage

	err := database.outbox.By("pending", kv.Text(channel)).Each(database.db, func(entry kv.Entry[outboxRecord]) (bool, error) {
		if !entry.Record.NextAttemptAt.After(now) {
			messages = append(messages, entry.Record.OutboxMessage)
		}

		return len(messages) < limit, nil
	})

	return messages, err
}

// changeOutbox rewrites a message in one batch; a deleted one is left alone.
func (database *Store) changeOutbox(id int64, change func(message *outboxRecord)) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		entry, err := database.outbox.GetEntry(batch, kv.K(kv.U64(uint64(id))))

		if err != nil {
			return ignoreMissing(err)
		}

		message := entry.Record
		change(&message)
		_, err = database.outbox.Write(batch, message, &entry)

		return err
	})
}

// ignoreMissing: a message deleted meanwhile (its connection went) needs no update.
func ignoreMissing(err error) error {
	if notFound(err) == model.ErrNotFound {
		return nil
	}

	return err
}

func (database *Store) MarkOutboxSent(_ context.Context, id int64, now time.Time) error {
	return database.changeOutbox(id, func(message *outboxRecord) {
		message.Status, message.Attempts, message.LastError, message.UpdatedAt = model.OutboxSent, message.Attempts+1, "", now.UTC()
	})
}

// MarkOutboxRetry records a failed attempt and when to try again.
func (database *Store) MarkOutboxRetry(_ context.Context, id int64, lastError string, next, now time.Time) error {
	return database.changeOutbox(id, func(message *outboxRecord) {
		message.Attempts, message.LastError, message.NextAttemptAt, message.UpdatedAt = message.Attempts+1, lastError, next.UTC(), now.UTC()
	})
}

// MarkOutboxFailed gives up on a message after its last attempt.
func (database *Store) MarkOutboxFailed(_ context.Context, id int64, lastError string, now time.Time) error {
	return database.changeOutbox(id, func(message *outboxRecord) {
		message.Status, message.Attempts, message.LastError, message.UpdatedAt = model.OutboxFailed, message.Attempts+1, lastError, now.UTC()
	})
}

// RetryOutboxNow makes the waiting messages of a channel due now: after its
// connection is fixed they need not wait for the next step of the schedule.
func (database *Store) RetryOutboxNow(_ context.Context, channel string, now time.Time) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		return database.outbox.By("pending", kv.Text(channel)).Each(batch, func(entry kv.Entry[outboxRecord]) (bool, error) {
			if !entry.Record.NextAttemptAt.After(now) {
				return true, nil
			}

			message := entry.Record
			message.NextAttemptAt = now.UTC()
			_, err := database.outbox.Write(batch, message, &entry)

			return true, err
		})
	})
}

// CleanOutbox deletes sent messages finished before sentBefore and failed ones
// before failedBefore, so the outbox does not grow forever.
func (database *Store) CleanOutbox(_ context.Context, sentBefore, failedBefore time.Time) (int64, error) {
	var deleted int64

	err := database.db.Update(true, func(batch *kv.Batch) error {
		for status, before := range map[string]time.Time{model.OutboxSent: sentBefore, model.OutboxFailed: failedBefore} {
			err := database.outbox.By("status", kv.Text(status)).Each(batch, func(entry kv.Entry[outboxRecord]) (bool, error) {
				if entry.Record.UpdatedAt.Before(before) {
					database.outbox.DeleteEntry(batch, entry)

					deleted++
				}

				return true, nil
			})

			if err != nil {
				return err
			}
		}

		return nil
	})

	return deleted, err
}

// PendingChannels lists the channels with messages still to send, so their workers start after a restart.
func (database *Store) PendingChannels(context.Context) ([]string, error) {
	var channels []string

	err := database.outbox.By("pending").Each(database.db, func(entry kv.Entry[outboxRecord]) (bool, error) {
		if len(channels) == 0 || channels[len(channels)-1] != entry.Record.Channel {
			channels = append(channels, entry.Record.Channel)
		}

		return true, nil
	})

	return channels, err
}

// outboxQuery walks the messages of a filter, newest first, each filter by an index.
func (database *Store) outboxQuery(filter model.OutboxFilter) kv.Query[outboxRecord] {
	switch {
	case filter.Status != "" && filter.Channel != "":
		return database.outbox.By("channel", kv.Text(filter.Channel)).
			Where(func(message outboxRecord) bool { return message.Status == filter.Status }).Newest()
	case filter.Status != "":
		return database.outbox.By("status", kv.Text(filter.Status)).Newest()
	case filter.Channel != "":
		return database.outbox.By("channel", kv.Text(filter.Channel)).Newest()
	}

	return database.outbox.Primary().Newest()
}

func recordOf(message outboxRecord) model.OutboxRecord {
	return model.OutboxRecord{
		ID: message.ID, Channel: message.Channel, Kind: message.Kind, Payload: message.Payload, Status: message.Status,
		Attempts: message.Attempts, LastError: message.LastError, CreatedAt: message.CreatedAt,
		NextAttemptAt: message.NextAttemptAt, UpdatedAt: message.UpdatedAt,
	}
}

// ListOutbox returns outbox messages for the delivery log, newest first.
func (database *Store) ListOutbox(_ context.Context, filter model.OutboxFilter) ([]model.OutboxRecord, error) {
	var (
		records []model.OutboxRecord
		skipped int
	)

	err := database.outboxQuery(filter).Each(database.db, func(entry kv.Entry[outboxRecord]) (bool, error) {
		if skipped < filter.Offset {
			skipped++

			return true, nil
		}

		records = append(records, recordOf(entry.Record))

		return filter.Limit <= 0 || len(records) < filter.Limit, nil
	})

	return records, err
}

// CountOutbox counts the messages ListOutbox would page through.
func (database *Store) CountOutbox(_ context.Context, filter model.OutboxFilter) (int, error) {
	count, _, err := database.outboxQuery(filter).Count(database.db, 0)

	return count, err
}

// DeliveryHealthByChannel sums up the delivery of every channel with messages.
// A sent message has no error, so the latest error belongs to a retrying or failed one.
func (database *Store) DeliveryHealthByChannel(context.Context) (map[string]model.DeliveryHealth, error) {
	health := map[string]model.DeliveryHealth{}

	err := database.outbox.Primary().Each(database.db, func(entry kv.Entry[outboxRecord]) (bool, error) {
		message, channel := entry.Record, health[entry.Record.Channel]

		switch message.Status {
		case model.OutboxPending:
			channel.Pending++

			if message.Attempts > 0 {
				channel.Retrying++
			}
		case model.OutboxFailed:
			channel.Failed++
		case model.OutboxSent:
			if message.UpdatedAt.After(channel.LastSentAt) {
				channel.LastSentAt = message.UpdatedAt
			}
		}

		if message.LastError != "" && message.UpdatedAt.After(channel.LastErrorAt) {
			channel.LastError, channel.LastErrorAt = message.LastError, message.UpdatedAt
		}

		health[message.Channel] = channel

		return true, nil
	})

	return health, err
}

// Outbox messages keep their payloads after a JSON head, so a walk does not
// decode them as base64.
func encodeOutbox(message outboxRecord) ([]byte, error) {
	head := message
	head.Payload, head.EventPayload = nil, nil
	tail := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(message.Payload)+len(message.EventPayload)), uint32(len(message.Payload)))

	return withTail(head, append(append(tail, message.Payload...), message.EventPayload...))
}

func decodeOutbox(value []byte) (outboxRecord, error) {
	var message outboxRecord

	tail, err := splitTail(value, &message)

	if err != nil {
		return message, err
	}

	if len(tail) < 4 || int(binary.BigEndian.Uint32(tail)) > len(tail)-4 {
		return message, errShortRecord
	}

	size := 4 + int(binary.BigEndian.Uint32(tail))
	message.Payload, message.EventPayload = tail[4:size], tail[size:]

	return message, nil
}
