package kvstore

import (
	"context"
	"fmt"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// Envelope is what one inbox entry holds, ready to store. Sequence 0 is
// not from the inbox (SaveEvent, SaveLogs).
type Envelope struct {
	Sequence       uint64
	InboxSize      int // bytes of the inbox entry, freed once stored
	Events         []model.NewEvent
	Logs           []model.NewLog
	LogsPerMessage int // keep the newest of a repeated message (0: all)
}

// Write stores envelopes in one batch, together with the deletion of their
// inbox entries, and returns what each event changed.
func (database *Store) Write(envelopes []Envelope) ([][]model.SaveResult, error) {
	results := make([][]model.SaveResult, len(envelopes))
	freed := 0

	err := database.update(func(writer *batchWriter) error {
		for index, envelope := range envelopes {
			for _, event := range envelope.Events {
				result, err := writer.event(event)

				if err != nil {
					return err
				}

				results[index] = append(results[index], result)
			}

			for _, record := range envelope.Logs {
				if err := writer.log(record, envelope.LogsPerMessage); err != nil {
					return err
				}
			}

			if envelope.Sequence != 0 {
				if err := database.inbox.Delete(writer.batch, InboxEntry{Sequence: envelope.Sequence}); err != nil {
					return err
				}

				freed += envelope.InboxSize
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	database.inboxBytes.Add(-int64(freed))

	return results, nil
}

// SaveEvent stores one event and creates or updates its issue.
func (database *Store) SaveEvent(_ context.Context, event model.NewEvent) (model.SaveResult, error) {
	results, err := database.Write([]Envelope{{Events: []model.NewEvent{event}}})

	if err != nil {
		return model.SaveResult{}, err
	}

	return results[0][0], nil
}

// SaveLogs stores logs; with keepPerMessage > 0 the older copies of each
// repeated message beyond it are deleted.
func (database *Store) SaveLogs(_ context.Context, logs []model.NewLog, keepPerMessage int) error {
	_, err := database.Write([]Envelope{{Logs: logs, LogsPerMessage: keepPerMessage}})

	return err
}

// SetIssueStatus changes an issue's status.
func (database *Store) SetIssueStatus(ctx context.Context, issueID int64, status string, actor model.Actor) error {
	_, err := database.SetIssuesStatus(ctx, []int64{issueID}, status, actor)

	return err
}

// SetIssuesStatus applies one status to a batch atomically: a missing id
// changes nothing (ErrNotFound). Only real changes are recorded, with actor.
func (database *Store) SetIssuesStatus(_ context.Context, issueIDs []int64, status string, actor model.Actor) ([]model.Issue, error) {
	ids, err := model.NormalizeIssueIDs(issueIDs)

	if err != nil {
		return nil, err
	}

	switch status {
	case model.StatusResolved, model.StatusIgnored, model.StatusUnresolved:
	default:
		return nil, fmt.Errorf("invalid status %q", status)
	}

	var issues []model.Issue

	err = database.update(func(writer *batchWriter) error {
		now := database.now().UTC()
		issues = make([]model.Issue, 0, len(ids))

		for _, id := range ids {
			issue, ok := writer.issue(id)

			if !ok {
				return model.ErrNotFound
			}

			if issue.Status != status {
				if err := writer.statusChange(issue, status, actor, "", now); err != nil {
					return err
				}

				issue.Status = status
			}

			switch {
			case status != model.StatusResolved:
				issue.ResolvedAt = nil
			case issue.ResolvedAt == nil:
				resolvedAt := now
				issue.ResolvedAt = &resolvedAt
			}

			issues = append(issues, issue.Issue)
		}

		return nil
	})

	return issues, err
}

// DeleteIssue removes the issue with its events and status history. If the
// error happens again, it opens a new issue.
func (database *Store) DeleteIssue(_ context.Context, issueID int64) error {
	return database.update(func(writer *batchWriter) error {
		issue, ok := writer.issue(issueID)

		if !ok {
			return model.ErrNotFound
		}

		err := database.events.Primary(kv.U64(uint64(issueID))).Each(database.db, func(entry kv.Entry[eventRecord]) (bool, error) {
			writer.deleteEvent(entry)

			return true, nil
		})

		if err != nil {
			return err
		}

		err = database.status.Primary(kv.U64(uint64(issueID))).Each(database.db, func(entry kv.Entry[statusRecord]) (bool, error) {
			database.status.DeleteEntry(writer.batch, entry)

			return true, nil
		})

		if err != nil {
			return err
		}

		delete(writer.issues, issueID)
		writer.deleted[issueID] = issue

		return nil
	})
}

// IssueStatusHistory lists the status changes of an issue, newest first.
func (database *Store) IssueStatusHistory(_ context.Context, issueID int64) ([]model.StatusChange, error) {
	changes := []model.StatusChange{}

	err := database.status.Primary(kv.U64(uint64(issueID))).Newest().Each(database.db, func(entry kv.Entry[statusRecord]) (bool, error) {
		changes = append(changes, entry.Record.StatusChange)

		return true, nil
	})

	return changes, err
}

// batchWriter collects one batch. Issues it changes are copies, put in
// memory only once the batch is written.
type batchWriter struct {
	database *Store
	batch    *kv.Batch
	issues   map[int64]*issueRecord // changed by this batch
	deleted  map[int64]*issueRecord
	created  map[string]*issueRecord // new, by fingerprint key
	messages map[string]*messageGroup
	// events written by this batch, per issue, oldest first: newer than the
	// stored ones, so a trim reaches them last
	written map[int64][]eventRecord
	// removed are the events this batch deletes: reads of the database
	// still see them until the batch is written, so a later trim skips them.
	removed map[string]bool
	// newMark is the time mark the batch writes, if any.
	newMark *timeMark
}

// update runs change in one batch of the database, then puts what it
// changed in memory. Nothing is written or kept when change fails.
func (database *Store) update(change func(writer *batchWriter) error) error {
	writer := &batchWriter{
		database: database, issues: map[int64]*issueRecord{}, deleted: map[int64]*issueRecord{}, created: map[string]*issueRecord{},
		messages: map[string]*messageGroup{}, written: map[int64][]eventRecord{}, removed: map[string]bool{},
	}

	err := database.db.Update(true, func(batch *kv.Batch) error {
		writer.batch = batch
		first := database.db.Peek()

		if err := change(writer); err != nil {
			return err
		}

		return writer.finish(first)
	})

	if err != nil {
		return err
	}

	writer.apply()

	return nil
}

// finish adds what the changes imply: trims of repeated logs, the changed
// issues and the time mark.
func (writer *batchWriter) finish(first uint64) error {
	if err := writer.trimMessages(); err != nil {
		return err
	}

	for _, issue := range writer.issues {
		if err := writer.database.issues.Save(writer.batch, *issue); err != nil {
			return err
		}
	}

	// By key: the stored value says what to take off the counter, while
	// this batch's copy of the issue has already lost its events.
	for _, issue := range writer.deleted {
		if _, err := writer.database.issues.DeleteKey(writer.batch, writer.database.issues.KeyOf(*issue)); err != nil {
			return err
		}
	}

	return writer.mark(writer.database.now(), first)
}

// apply puts the written issues and mark in memory.
func (writer *batchWriter) apply() {
	if writer.newMark != nil {
		writer.database.addMark(*writer.newMark)
	}

	writer.database.issuesMutex.Lock()
	defer writer.database.issuesMutex.Unlock()

	for _, issue := range writer.issues {
		writer.database.table.put(issue)
	}

	for id := range writer.deleted {
		writer.database.table.remove(id)
	}
}

// issue returns this batch's copy of an issue.
func (writer *batchWriter) issue(id int64) (*issueRecord, bool) {
	if issue, ok := writer.issues[id]; ok {
		return issue, true
	}

	writer.database.issuesMutex.RLock()
	stored, ok := writer.database.table.byID[id]
	writer.database.issuesMutex.RUnlock()

	if !ok {
		return nil, false
	}

	issue := stored.copy()
	writer.issues[id] = issue

	return issue, true
}

func (writer *batchWriter) statusChange(issue *issueRecord, to string, actor model.Actor, eventID string, at time.Time) error {
	sequence, err := writer.database.db.Next()

	if err != nil {
		return err
	}

	return writer.database.status.Insert(writer.batch, statusRecord{
		IssueID: issue.ID, Sequence: sequence,
		StatusChange: model.StatusChange{Actor: actor, From: issue.Status, To: to, EventID: eventID, CreatedAt: at.UTC()},
	})
}

// findIssue is this batch's issue of a fingerprint, or nil for a new one.
func (writer *batchWriter) findIssue(fingerprint string) *issueRecord {
	if issue := writer.created[fingerprint]; issue != nil {
		return issue
	}

	writer.database.issuesMutex.RLock()
	existing := writer.database.table.byFingerprint[fingerprint]
	writer.database.issuesMutex.RUnlock()

	if existing == nil {
		return nil
	}

	issue, _ := writer.issue(existing.ID)

	return issue
}

func (writer *batchWriter) event(event model.NewEvent) (model.SaveResult, error) {
	var result model.SaveResult

	// A retry of the SDK, also one earlier in this batch.
	if taken, err := writer.database.events.Taken(writer.batch, "id", eventIDKey(event.ProjectID, event.EventID)); err != nil || taken {
		result.Duplicate = taken

		return result, err
	}

	received := event.ReceivedAt.UTC()
	fingerprint := fingerprintKey(event.ProjectID, event.Fingerprint)
	issue := writer.findIssue(fingerprint)

	switch {
	case issue == nil:
		writer.database.issuesMutex.Lock()
		writer.database.table.lastID++
		id := writer.database.table.lastID
		writer.database.issuesMutex.Unlock()

		issue = &issueRecord{
			Issue:       model.Issue{ID: id, ProjectID: event.ProjectID, Status: model.StatusUnresolved, FirstSeen: received},
			Fingerprint: event.Fingerprint,
		}
		writer.issues[id] = issue
		writer.created[fingerprint] = issue
		result.NewIssue = true
	case issue.Status == model.StatusResolved:
		// Recorded while the issue is still resolved; ignored issues stay ignored.
		if err := writer.statusChange(issue, model.StatusUnresolved, model.Actor{Source: model.SourceEvent}, event.EventID, received); err != nil {
			return result, err
		}

		issue.Status = model.StatusUnresolved
		issue.ResolvedAt = nil
		result.Regression = true
	}

	// Title, culprit and level follow the latest event.
	issue.Title, issue.Culprit = event.IssueText()
	issue.Level = event.Level
	issue.EventCount++

	if received.After(issue.LastSeen) {
		issue.LastSeen = received
	}

	issue.addEvent(received)
	result.IssueID = issue.ID

	sequence, err := writer.database.db.Next()

	if err != nil {
		return result, err
	}

	record := eventRecord{
		eventMeta: eventMeta{
			Sequence: sequence, ProjectID: event.ProjectID, IssueID: issue.ID, EventID: event.EventID, Level: event.Level,
			Environment: event.Environment, Release: event.Release, TraceID: event.TraceID, SpanID: event.SpanID,
			ParentSpanID: event.ParentSpanID, Timestamp: event.Timestamp.UTC(), ReceivedAt: received,
		},
		Payload: event.Payload,
	}

	if err := writer.database.events.Insert(writer.batch, record); err != nil {
		return result, err
	}

	writer.written[issue.ID] = append(writer.written[issue.ID], record)

	if event.KeepEvents > 0 && issue.Stored > event.KeepEvents {
		if err := writer.trimIssue(issue, event.KeepEvents); err != nil {
			return result, err
		}
	}

	return result, nil
}

// deleteEvent deletes a stored event with its index keys, once per batch,
// and takes it off its issue's counts.
func (writer *batchWriter) deleteEvent(entry kv.Entry[eventRecord]) {
	if writer.removed[string(entry.Key)] {
		return
	}

	writer.removed[string(entry.Key)] = true
	writer.database.events.DeleteEntry(writer.batch, entry)

	if issue, ok := writer.issue(entry.Record.IssueID); ok {
		issue.removeEvent(entry.Record.ReceivedAt)
	}
}

// trimIssue deletes the oldest events of an issue beyond keep: the stored
// ones first, then the ones this batch wrote.
func (writer *batchWriter) trimIssue(issue *issueRecord, keep int) error {
	err := writer.database.events.Primary(kv.U64(uint64(issue.ID))).Each(writer.database.db, func(entry kv.Entry[eventRecord]) (bool, error) {
		writer.deleteEvent(entry)

		return issue.Stored > keep, nil
	})

	if err != nil {
		return err
	}

	for issue.Stored > keep && len(writer.written[issue.ID]) > 0 {
		oldest := writer.written[issue.ID][0]
		writer.written[issue.ID] = writer.written[issue.ID][1:]

		if err := writer.database.events.Delete(writer.batch, oldest); err != nil {
			return err
		}

		writer.removed[string(writer.database.events.KeyOf(oldest))] = true
		issue.removeEvent(oldest.ReceivedAt)
	}

	return nil
}

// messageGroup is the logs of one repeated message written by this batch.
type messageGroup struct {
	projectID   int
	fingerprint string
	added       []logRecord
	keep        int
}

func (writer *batchWriter) log(record model.NewLog, keepPerMessage int) error {
	sequence, err := writer.database.db.Next()

	if err != nil {
		return err
	}

	stored := logRecord{
		logMeta: logMeta{
			Sequence: sequence, ReceivedDay: dayOf(writer.database.now()), ProjectID: record.ProjectID, Timestamp: record.Timestamp.UTC(),
			Level: record.Level, SeverityNumber: record.SeverityNumber, Body: record.Body, TraceID: record.TraceID,
			SpanID: record.SpanID, Fingerprint: record.Fingerprint,
		},
		Attributes: record.Attributes,
	}

	if err := writer.database.logs.Insert(writer.batch, stored); err != nil {
		return err
	}

	if record.Fingerprint != "" && keepPerMessage > 0 {
		key := fingerprintKey(record.ProjectID, record.Fingerprint)
		group := writer.messages[key]

		if group == nil {
			group = &messageGroup{projectID: record.ProjectID, fingerprint: record.Fingerprint, keep: keepPerMessage}
			writer.messages[key] = group
		}

		group.added = append(group.added, stored)
	}

	return nil
}

// trimMessages keeps the newest copies of each repeated message: the oldest
// stored ones go first, then the oldest of this batch.
func (writer *batchWriter) trimMessages() error {
	for _, group := range writer.messages {
		var stored []kv.Entry[logRecord] // oldest first

		query := writer.database.logs.By("message", messageParts(group.projectID, group.fingerprint)...)

		err := query.Each(writer.database.db, func(entry kv.Entry[logRecord]) (bool, error) {
			stored = append(stored, entry)

			return true, nil
		})

		if err != nil {
			return err
		}

		excess := len(stored) + len(group.added) - group.keep

		for _, entry := range stored[:max(0, min(excess, len(stored)))] {
			writer.database.logs.DeleteEntry(writer.batch, entry)
		}

		for _, record := range group.added[:max(0, min(excess-len(stored), len(group.added)))] {
			if err := writer.database.logs.Delete(writer.batch, record); err != nil {
				return err
			}
		}
	}

	return nil
}
