package database

import (
	"context"
	"fmt"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// Envelope is an inbox entry ready to store; Sequence 0 is not from the inbox.
type Envelope struct {
	Sequence       uint64
	InboxSize      int // bytes of the inbox entry, freed once stored
	Events         []model.NewEvent
	Logs           []model.NewLog
	LogsPerMessage int // keep the newest of a repeated message (0: all)
}

// Write stores envelopes and deletes their inbox entries in one batch.
func (database *Store) Write(envelopes []Envelope) ([][]model.SaveResult, error) {
	results := make([][]model.SaveResult, len(envelopes))
	freed := 0

	// Copies of a repeated message this batch would delete are not written.
	copies := map[string]int{}

	for _, envelope := range envelopes {
		for _, record := range envelope.Logs {
			if record.Fingerprint != "" && envelope.LogsPerMessage > 0 {
				copies[fingerprintKey(record.ProjectID, record.Fingerprint)]++
			}
		}
	}

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
				if record.Fingerprint != "" && envelope.LogsPerMessage > 0 {
					group := fingerprintKey(record.ProjectID, record.Fingerprint)

					if copies[group]--; copies[group] >= envelope.LogsPerMessage {
						continue
					}
				}

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

func (database *Store) SaveEvent(_ context.Context, event model.NewEvent) (model.SaveResult, error) {
	results, err := database.Write([]Envelope{{Events: []model.NewEvent{event}}})

	if err != nil {
		return model.SaveResult{}, err
	}

	return results[0][0], nil
}

func (database *Store) SaveLogs(_ context.Context, logs []model.NewLog, keepPerMessage int) error {
	_, err := database.Write([]Envelope{{Logs: logs, LogsPerMessage: keepPerMessage}})

	return err
}

func (database *Store) SetIssuesStatus(_ context.Context, issueIDs []string, status string, actor model.Actor) ([]model.Issue, error) {
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

// DeleteIssues deletes the issues that exist, several per batch of up to
// cleanupChunk records; an issue goes with all its events in one batch.
func (database *Store) DeleteIssues(ctx context.Context, issueIDs []string) (int, error) {
	deleted := 0

	for len(issueIDs) > 0 && ctx.Err() == nil {
		taken, gone := 0, 0

		err := database.update(func(writer *batchWriter) error {
			for records := 0; taken < len(issueIDs) && records < cleanupChunk; {
				issue, ok := writer.issue(issueIDs[taken])
				taken++

				if !ok || writer.deleted[issue.ID] != nil {
					continue
				}

				records += issue.Stored + 1

				if err := writer.deleteIssueEvents(issue); err != nil {
					return err
				}

				if err := writer.deleteIssue(issue); err != nil {
					return err
				}

				gone++
			}

			return nil
		})

		if err != nil {
			return deleted, err
		}

		deleted += gone
		issueIDs = issueIDs[taken:]
	}

	return deleted, ctx.Err()
}

func (writer *batchWriter) deleteIssueEvents(issue *issueRecord) error {
	return writer.database.events.Primary(kv.Text(issue.ID)).Head().Each(writer.database.db, func(entry kv.Entry[eventRecord]) (bool, error) {
		writer.deleteEvent(entry)

		return true, nil
	})
}

// deleteIssue deletes an issue and its history; its events must be gone.
func (writer *batchWriter) deleteIssue(issue *issueRecord) error {
	err := writer.database.status.Primary(kv.Text(issue.ID)).Each(writer.database.db, func(entry kv.Entry[statusRecord]) (bool, error) {
		writer.database.status.DeleteEntry(writer.batch, entry)

		return true, nil
	})

	delete(writer.issues, issue.ID)
	writer.deleted[issue.ID] = issue
	writer.count(issue.ProjectID).Issues++

	return err
}

func (database *Store) IssueStatusHistory(_ context.Context, issueID string) ([]model.StatusChange, error) {
	changes := []model.StatusChange{}

	err := database.status.Primary(kv.Text(issueID)).Newest().Each(database.db, func(entry kv.Entry[statusRecord]) (bool, error) {
		changes = append(changes, entry.Record.StatusChange)

		return true, nil
	})

	return changes, err
}

// batchWriter collects one batch; the issues it changes are copies until it is written.
type batchWriter struct {
	database *Store
	batch    *kv.Batch
	issues   map[string]*issueRecord // changed by this batch
	stored   map[string]*issueRecord // their versions as stored, for Write
	deleted  map[string]*issueRecord
	created  map[string]*issueRecord // new, by fingerprint key
	messages map[string]*messageGroup
	// removed are events this batch deletes; reads still see them until it is written.
	removed      map[string]bool
	newMark      *timeMark
	messageStart map[string]uint64
	// counted is what this batch deletes per project, inside the cleanup.
	counted map[int]*CleanupCounts
}

// update runs change in one batch; its changes reach memory before the next batch.
func (database *Store) update(change func(writer *batchWriter) error) error {
	writer := &batchWriter{
		database: database, issues: map[string]*issueRecord{}, stored: map[string]*issueRecord{}, deleted: map[string]*issueRecord{},
		created:  map[string]*issueRecord{},
		messages: map[string]*messageGroup{}, removed: map[string]bool{},
		messageStart: map[string]uint64{},
	}

	err := database.db.Update(true, func(batch *kv.Batch) error {
		writer.batch = batch
		first := database.db.Peek()

		if err := change(writer); err != nil {
			return err
		}

		batch.AfterWrite(writer.apply)

		return writer.finish(first)
	})

	if err != nil {
		return err
	}

	database.noteWritten()

	return nil
}

// finish adds the trims of repeated logs, the changed issues and the time mark.
func (writer *batchWriter) finish(first uint64) error {
	if err := writer.trimMessages(); err != nil {
		return err
	}

	for id, issue := range writer.issues {
		var previous *kv.Entry[issueRecord]

		if stored := writer.stored[id]; stored != nil {
			previous = &kv.Entry[issueRecord]{Record: *stored, Size: stored.storedSize}
		}

		written, err := writer.database.issues.Write(writer.batch, *issue, previous)

		if err != nil {
			return err
		}

		issue.storedSize = written.Size
	}

	// By key: the copy has lost its events, the stored value has the size.
	for _, issue := range writer.deleted {
		if _, err := writer.database.issues.DeleteKey(writer.batch, writer.database.issues.KeyOf(*issue)); err != nil {
			return err
		}
	}

	return writer.mark(writer.database.now(), first)
}

func (writer *batchWriter) apply() {
	if writer.newMark != nil {
		writer.database.addMark(*writer.newMark)
	}

	if len(writer.database.messageStart) > maxMessageStarts {
		clear(writer.database.messageStart)
	}

	for group, start := range writer.messageStart {
		writer.database.messageStart[group] = start
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

// newIssueID returns a random ID no issue has, stored or new in this batch.
func (writer *batchWriter) newIssueID() string {
	for {
		id := model.NewID()

		writer.database.issuesMutex.RLock()
		_, stored := writer.database.table.byID[id]
		writer.database.issuesMutex.RUnlock()

		if _, created := writer.issues[id]; !stored && !created {
			return id
		}
	}
}

func (writer *batchWriter) issue(id string) (*issueRecord, bool) {
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
	writer.stored[id] = stored

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

// findIssue returns nil for a new issue.
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

	if taken, err := writer.database.events.Taken(writer.batch, "id", eventIDKey(event.ProjectID, event.EventID)); err != nil || taken {
		result.Duplicate = taken

		return result, err
	}

	received := event.ReceivedAt.UTC()
	fingerprint := fingerprintKey(event.ProjectID, event.Fingerprint)
	issue := writer.findIssue(fingerprint)

	switch {
	case issue == nil:
		id := writer.newIssueID()

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

	return result, nil
}

// deleteEvent deletes a stored event once per batch and uncounts it.
func (writer *batchWriter) deleteEvent(entry kv.Entry[eventRecord]) {
	if writer.removed[string(entry.Key)] {
		return
	}

	writer.removed[string(entry.Key)] = true
	writer.database.events.DeleteEntry(writer.batch, entry)
	writer.count(entry.Record.ProjectID).Events++

	if issue, ok := writer.issue(entry.Record.IssueID); ok {
		issue.removeEvent(entry.Record.ReceivedAt)
	}
}

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

// trimMessages keeps the newest copies of each repeated message.
func (writer *batchWriter) trimMessages() error {
	for name, group := range writer.messages {
		var stored []kv.Key // oldest first

		query := writer.database.logs.By("message", messageParts(group.projectID, group.fingerprint)...)

		if start := writer.database.messageStart[name]; start > 0 {
			query = query.From(kv.U64(start))
		}

		err := query.Keys(writer.database.db, func(key kv.Key) (bool, error) {
			stored = append(stored, key)

			return true, nil
		})

		if err != nil {
			return err
		}

		excess := len(stored) + len(group.added) - group.keep

		for _, key := range stored[:max(0, min(excess, len(stored)))] {
			if _, err := writer.database.logs.DeleteKey(writer.batch, key); err != nil {
				return err
			}
		}

		for _, record := range group.added[:max(0, min(excess-len(stored), len(group.added)))] {
			if err := writer.database.logs.Delete(writer.batch, record); err != nil {
				return err
			}
		}

		switch kept := max(excess, 0); {
		case kept < len(stored):
			writer.messageStart[name] = sequenceOf(stored[kept])
		case kept-len(stored) < len(group.added):
			writer.messageStart[name] = group.added[kept-len(stored)].Sequence
		}
	}

	return nil
}

// maxMessageStarts bounds messageStart (about 50 bytes each).
const maxMessageStarts = 100_000
