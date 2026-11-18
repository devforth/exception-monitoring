package kvstore

import (
	"errors"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// The inbox keeps envelopes as they arrived, until the pipeline of ingest
// has stored what they hold: the HTTP handler answers once an envelope is
// here. An entry is deleted in the batch that stores its data, so a crash
// neither loses nor repeats one.

// Append adds an envelope and returns its entry once it is on disk.
// Concurrent appends share their disk syncs (the store commits them as a
// group), and none waits for the writer.
func (database *Store) Append(projectID int, received time.Time, body []byte) (InboxEntry, error) {
	sequence, err := database.db.Next()

	if err != nil {
		return InboxEntry{}, err
	}

	entry := InboxEntry{Sequence: sequence, ProjectID: projectID, Received: received.UTC(), Body: body}
	batch := kv.NewBatch(database.db)

	if err := database.inbox.Insert(batch, entry); err != nil {
		return InboxEntry{}, err
	}

	if err := database.db.Write(batch, true); err != nil {
		return InboxEntry{}, err
	}

	entry.Size = inboxHeader + len(body)
	database.inboxBytes.Add(int64(entry.Size))

	return entry, nil
}

// InInbox reports whether an entry is still waiting (not stored yet).
func (database *Store) InInbox(sequence uint64) (bool, error) {
	_, err := database.inbox.Get(database.db, kv.K(kv.U64(sequence)))

	if errors.Is(err, kv.ErrNotFound) {
		return false, nil
	}

	return err == nil, err
}

// InboxBytes is the size of the envelopes waiting.
func (database *Store) InboxBytes() int64 {
	return database.inboxBytes.Load()
}

// Pending returns up to limit entries after the given sequence number,
// oldest first, without the ones skip names.
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
