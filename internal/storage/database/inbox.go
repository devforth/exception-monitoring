package database

import (
	"errors"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// Append writes without a sync: the writer's next synced batch takes it to disk.
func (database *Store) Append(projectID int, received time.Time, body []byte) (InboxEntry, error) {
	sequence, err := database.db.Next()

	if err != nil {
		return InboxEntry{}, err
	}

	entry := InboxEntry{Sequence: sequence, ProjectID: projectID, Received: received.UTC(), Body: body}
	batch := kv.NewBatch(database.db.Engine)

	if err := database.inbox.Insert(batch, entry); err != nil {
		return InboxEntry{}, err
	}

	if err := database.db.Write(batch, false); err != nil {
		return InboxEntry{}, err
	}

	entry.Size = inboxHeader + len(body)
	database.inboxBytes.Add(int64(entry.Size))

	return entry, nil
}

// InInbox reports whether an entry is still waiting.
func (database *Store) InInbox(sequence uint64) (bool, error) {
	_, err := database.inbox.Get(database.db, kv.K(kv.U64(sequence)))

	if errors.Is(err, kv.ErrNotFound) {
		return false, nil
	}

	return err == nil, err
}

// InboxBytes returns the size of the waiting envelopes.
func (database *Store) InboxBytes() int64 {
	return database.inboxBytes.Load()
}

// Pending returns up to limit entries after the sequence number, oldest first, except skipped ones.
func (database *Store) Pending(after uint64, limit int, skip func(sequence uint64) bool) ([]InboxEntry, error) {
	var entries []InboxEntry

	query := database.inbox.Primary().From(kv.U64(after + 1))

	err := query.Each(database.db, func(entry kv.Entry[InboxEntry]) (bool, error) {
		if skip == nil || !skip(entry.Record.Sequence) {
			entries = append(entries, entry.Record)
		}

		return len(entries) < limit, nil
	})

	return entries, err
}

func (database *Store) loadInboxSize() error {
	var size int64

	err := database.inbox.Primary().Each(database.db, func(entry kv.Entry[InboxEntry]) (bool, error) {
		size += int64(entry.Size)

		return true, nil
	})

	database.inboxBytes.Store(size)

	return err
}
