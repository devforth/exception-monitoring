package database

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/devforth/exception-monitoring/internal/platform/text"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// newestFirst walks several queries at once, newest first across all of
// them, by the sequence number that ends the keys of events and logs.
// visit returns false to stop.
func newestFirst[T any](reader kv.Reader, queries []kv.Query[T], visit func(entry kv.Entry[T]) bool) error {
	type head struct {
		cursor *kv.Cursor[T]
		entry  kv.Entry[T]
		valid  bool
	}

	heads := make([]*head, 0, len(queries))

	for _, query := range queries {
		current := &head{cursor: query.Newest().Open(reader)}
		current.entry, current.valid = current.cursor.Next()
		heads = append(heads, current)
	}

	for {
		var newest *head

		for _, current := range heads {
			if current.valid && (newest == nil || sequenceOf(current.entry.Key) > sequenceOf(newest.entry.Key)) {
				newest = current
			}
		}

		if newest == nil {
			break
		}

		if !visit(newest.entry) {
			break
		}

		newest.entry, newest.valid = newest.cursor.Next()
	}

	var err error

	for _, current := range heads {
		err = errors.Join(err, current.cursor.Close())
	}

	return err
}

// before bounds a query to sequence numbers below before (0: no bound).
func before[T any](query kv.Query[T], sequence uint64) kv.Query[T] {
	if sequence == 0 {
		return query
	}

	return query.Before(kv.U64(sequence))
}

func eventSummary(meta eventMeta) model.EventSummary {
	return model.EventSummary{
		ID: int64(meta.Sequence), ProjectID: meta.ProjectID, IssueID: meta.IssueID, EventID: meta.EventID, Timestamp: meta.Timestamp,
		ReceivedAt: meta.ReceivedAt, Level: meta.Level, Environment: meta.Environment, Release: meta.Release,
		TraceID: meta.TraceID, SpanID: meta.SpanID, ParentSpanID: meta.ParentSpanID,
	}
}

func fullEvent(record eventRecord) model.Event {
	return model.Event{EventSummary: eventSummary(record.eventMeta), Payload: json.RawMessage(record.Payload)}
}

// ListEvents lists events newest first: of an issue, of a trace, or all.
func (database *Store) ListEvents(_ context.Context, filter model.EventFilter) ([]model.EventSummary, error) {
	limit := uint64(max(filter.BeforeID, 0))

	var queries []kv.Query[eventRecord]

	switch {
	case filter.TraceID != "":
		queries = append(queries, database.events.By("trace", kv.Text(filter.TraceID)))
	case filter.IssueID != 0:
		queries = append(queries, database.events.Primary(kv.U64(uint64(filter.IssueID))))
	default:
		for _, rank := range allRanks {
			queries = append(queries, database.events.By("age", kv.U8(rank)))
		}
	}

	for index := range queries {
		queries[index] = before(queries[index], limit)
	}

	events := []model.EventSummary{}

	err := newestFirst(database.db, queries, func(entry kv.Entry[eventRecord]) bool {
		if filter.IssueID == 0 || entry.Record.IssueID == filter.IssueID {
			events = append(events, eventSummary(entry.Record.eventMeta))
		}

		return filter.Limit <= 0 || len(events) < filter.Limit
	})

	return events, err
}

// GetEvent returns an event by its SDK event_id.
func (database *Store) GetEvent(_ context.Context, projectID int, eventID string) (model.Event, error) {
	record, err := database.events.GetBy(database.db, "id", eventIDKey(projectID, eventID))

	if err != nil {
		return model.Event{}, notFound(err)
	}

	return fullEvent(record), nil
}

// LatestEvent returns the most recent event of an issue.
func (database *Store) LatestEvent(_ context.Context, issueID int64) (model.Event, error) {
	entry, err := database.events.Primary(kv.U64(uint64(issueID))).Newest().First(database.db)

	if err != nil {
		return model.Event{}, notFound(err)
	}

	return fullEvent(entry.Record), nil
}

// logQueries picks the walks a log filter reads: the trace's, the spans',
// the projects' at the filter's levels, or every level.
func (database *Store) logQueries(filter model.LogFilter) []kv.Query[logRecord] {
	limit := uint64(max(filter.BeforeID, 0))

	// Logs received after the range (with some slack) cannot be in it.
	if !filter.Until.IsZero() {
		if end := database.sequenceAfter(filter.Until.Add(markSlack)); end > 0 && (limit == 0 || end < limit) {
			limit = end
		}
	}

	var queries []kv.Query[logRecord]

	switch {
	case filter.TraceID != "":
		queries = append(queries, database.logs.By("trace", kv.Text(filter.TraceID)))
	case len(filter.SpanIDs) > 0:
		// A span named twice would list its logs twice.
		spans := slices.Clone(filter.SpanIDs)
		slices.Sort(spans)

		for _, span := range slices.Compact(spans) {
			queries = append(queries, database.logs.By("span", kv.Text(span)))
		}
	case len(filter.ProjectIDs) > 0:
		for _, project := range filter.ProjectIDs {
			for _, rank := range ranksOf(filter.Levels) {
				queries = append(queries, database.logs.By("project", kv.U32(uint32(project)), kv.U8(rank)))
			}
		}
	default:
		for _, rank := range ranksOf(filter.Levels) {
			queries = append(queries, database.logs.Primary(kv.U8(rank)))
		}
	}

	for index := range queries {
		queries[index] = before(queries[index], limit)
	}

	return queries
}

// logMatches checks the conditions the chosen walks do not.
func logMatches(record logRecord, filter model.LogFilter) bool {
	switch {
	case len(filter.ProjectIDs) > 0 && !slices.Contains(filter.ProjectIDs, record.ProjectID),
		len(filter.Levels) > 0 && !slices.Contains(filter.Levels, record.Level),
		filter.TraceID != "" && record.TraceID != filter.TraceID,
		len(filter.SpanIDs) > 0 && !slices.Contains(filter.SpanIDs, record.SpanID),
		!filter.Since.IsZero() && record.Timestamp.Before(filter.Since),
		!filter.Until.IsZero() && !record.Timestamp.Before(filter.Until),
		filter.Query != "" && !text.ContainsFold(record.Body, filter.Query):
		return false
	}

	return true
}

// scanLogs visits the logs matching the filter, newest first, until visit
// returns false. The walks never name a log twice: a log has one trace, one
// span, one project and one level.
func (database *Store) scanLogs(filter model.LogFilter, visit func(record logRecord) bool) error {
	// Logs received well before the range cannot be in it: the scan stops
	// at the first of them.
	var stop uint64

	if !filter.Since.IsZero() {
		stop = database.sequenceAt(filter.Since.Add(-markSlack))
	}

	return newestFirst(database.db, database.logQueries(filter), func(entry kv.Entry[logRecord]) bool {
		if entry.Record.Sequence < stop {
			return false
		}

		return !logMatches(entry.Record, filter) || visit(entry.Record)
	})
}

func logView(record logRecord) model.LogRecord {
	return model.LogRecord{
		ID: int64(record.Sequence), ProjectID: record.ProjectID, Timestamp: record.Timestamp, Level: record.Level,
		SeverityNumber: record.SeverityNumber, Body: record.Body, TraceID: record.TraceID, SpanID: record.SpanID,
		Attributes: json.RawMessage(record.Attributes),
	}
}

// ListLogs returns a page of the logs matching the filter, newest first.
func (database *Store) ListLogs(_ context.Context, filter model.LogFilter) ([]model.LogRecord, error) {
	logs := []model.LogRecord{}
	skip := max(filter.Offset, 0)

	err := database.scanLogs(filter, func(record logRecord) bool {
		if skip > 0 {
			skip--

			return true
		}

		logs = append(logs, logView(record))

		return filter.Limit <= 0 || len(logs) < filter.Limit
	})

	return logs, err
}

// CountLogs counts the logs matching the filter up to LogCountLimit; more
// says there are more than that.
func (database *Store) CountLogs(_ context.Context, filter model.LogFilter) (int, bool, error) {
	count := 0

	err := database.scanLogs(filter, func(logRecord) bool {
		count++

		return count <= model.LogCountLimit
	})

	if count > model.LogCountLimit {
		return model.LogCountLimit, true, err
	}

	return count, false, err
}

// IssuesForLogs finds, for each log, the issue of an error of the same
// request: an event of the same trace whose span or parent span is the log's.
func (database *Store) IssuesForLogs(_ context.Context, logs []model.LogRecord) (map[int64]model.LogIssue, error) {
	result := map[int64]model.LogIssue{}
	bySpan := map[string]model.LogIssue{} // trace + "/" + span
	looked := map[string]bool{}

	for _, record := range logs {
		if record.SpanID == "" || looked[record.SpanID] {
			continue
		}

		looked[record.SpanID] = true

		queries := []kv.Query[eventRecord]{
			database.events.By("span", kv.Text(record.SpanID)), database.events.By("parent", kv.Text(record.SpanID)),
		}

		err := newestFirst(database.db, queries, func(entry kv.Entry[eventRecord]) bool {
			meta := entry.Record

			database.issuesMutex.RLock()
			issue, ok := database.table.byID[meta.IssueID]
			database.issuesMutex.RUnlock()

			if ok {
				for _, span := range []string{meta.SpanID, meta.ParentSpanID} {
					if key := meta.TraceID + "/" + span; span != "" && bySpan[key] == (model.LogIssue{}) {
						bySpan[key] = model.LogIssue{IssueID: issue.ID, Title: issue.Title}
					}
				}
			}

			return true
		})

		if err != nil {
			return nil, err
		}
	}

	for _, record := range logs {
		if issue, ok := bySpan[record.TraceID+"/"+record.SpanID]; ok && record.SpanID != "" {
			result[record.ID] = issue
		}
	}

	return result, nil
}

// ranksOf are the ranks of the levels a filter names (all for none).
func ranksOf(levels []string) []byte {
	if len(levels) == 0 {
		return allRanks
	}

	var ranks []byte

	for _, level := range levels {
		if rank := levelRank(level); !slices.Contains(ranks, rank) {
			ranks = append(ranks, rank)
		}
	}

	return ranks
}
