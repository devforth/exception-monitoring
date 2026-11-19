package database

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/text"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// newestFirst merges queries newest first by the sequence number ending their keys.
func newestFirst[T any](reader kv.Reader, queries []kv.Query[T], visit func(entry kv.Entry[T]) bool) error {
	return merged(reader, queries, true, visit)
}

// oldestFirst merges queries oldest first by the sequence number ending their keys.
func oldestFirst[T any](reader kv.Reader, queries []kv.Query[T], visit func(entry kv.Entry[T]) bool) error {
	return merged(reader, queries, false, visit)
}

func merged[T any](reader kv.Reader, queries []kv.Query[T], newest bool, visit func(entry kv.Entry[T]) bool) error {
	type head struct {
		cursor *kv.Cursor[T]
		entry  kv.Entry[T]
		valid  bool
	}

	heads := make([]*head, 0, len(queries))

	for _, query := range queries {
		if newest {
			query = query.Newest()
		}

		current := &head{cursor: query.Open(reader)}
		current.entry, current.valid = current.cursor.Next()
		heads = append(heads, current)
	}

	for {
		var next *head

		for _, current := range heads {
			if !current.valid {
				continue
			}

			if next == nil || (sequenceOf(current.entry.Key) > sequenceOf(next.entry.Key)) == newest {
				next = current
			}
		}

		if next == nil || !visit(next.entry) {
			break
		}

		next.entry, next.valid = next.cursor.Next()
	}

	var err error

	for _, current := range heads {
		err = errors.Join(err, current.cursor.Close())
	}

	return err
}

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
		TraceID: meta.TraceID, SpanID: meta.SpanID, ParentSpanID: meta.ParentSpanID, Thinned: meta.Thinned,
	}
}

func fullEvent(record eventRecord) model.Event {
	return model.Event{EventSummary: eventSummary(record.eventMeta), Payload: json.RawMessage(record.Payload)}
}

func (database *Store) ListEvents(_ context.Context, filter model.EventFilter) ([]model.EventSummary, error) {
	limit := uint64(max(filter.BeforeID, 0))

	var queries []kv.Query[eventRecord]

	switch {
	case filter.TraceID != "":
		queries = append(queries, database.events.By("trace", kv.Text(filter.TraceID)))
	case filter.IssueID != "":
		queries = append(queries, database.events.Primary(kv.Text(filter.IssueID)))
	default:
		for _, rank := range allRanks {
			queries = append(queries, database.events.By("age", kv.U8(rank)))
		}
	}

	for index := range queries {
		queries[index] = before(queries[index], limit).Head()
	}

	events := []model.EventSummary{}

	err := newestFirst(database.db, queries, func(entry kv.Entry[eventRecord]) bool {
		if filter.IssueID == "" || entry.Record.IssueID == filter.IssueID {
			events = append(events, eventSummary(entry.Record.eventMeta))
		}

		return filter.Limit <= 0 || len(events) < filter.Limit
	})

	return events, err
}

func (database *Store) GetEvent(_ context.Context, projectID int, eventID string) (model.Event, error) {
	record, err := database.events.GetBy(database.db, "id", eventIDKey(projectID, eventID))

	if err != nil {
		return model.Event{}, notFound(err)
	}

	return fullEvent(record), nil
}

func (database *Store) LatestEvent(_ context.Context, issueID string) (model.Event, error) {
	entry, err := database.events.Primary(kv.Text(issueID)).Newest().First(database.db)

	if err != nil {
		return model.Event{}, notFound(err)
	}

	return fullEvent(entry.Record), nil
}

// logQueries picks the walks a log filter reads.
func (database *Store) logQueries(filter model.LogFilter) []kv.Query[logRecord] {
	limit := uint64(max(filter.BeforeID, 0))

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

// scanLogs visits matching logs newest first until visit returns false.
func (database *Store) scanLogs(filter model.LogFilter, visit func(record logRecord) bool) error {
	queries := database.logQueries(filter)

	for index := range queries {
		if !filter.Since.IsZero() {
			queries[index] = queries[index].From(kv.U64(database.sequenceAt(filter.Since.Add(-markSlack))))
		}

		if rawSearchable(filter.Query) {
			queries[index] = queries[index].Raw(func(value []byte) bool { return text.ContainsFold(value, filter.Query) })
		}
	}

	return newestFirst(database.db, queries, func(entry kv.Entry[logRecord]) bool {
		return !logMatches(entry.Record, filter) || visit(entry.Record)
	})
}

// rawSearchable: the text appears as is in a stored log, since JSON escapes
// none of its characters, so the stored bytes can be searched before decoding.
func rawSearchable(query string) bool {
	if query == "" {
		return false
	}

	for _, char := range query {
		if char < ' ' || strings.ContainsRune(`"\<>&`, char) || char == '\u2028' || char == '\u2029' {
			return false
		}
	}

	return true
}

// countByKeys: the walks of the filter select exactly the logs it means, so
// counting their keys is enough.
func countByKeys(filter model.LogFilter) bool {
	exact := filter.Query == "" && filter.Since.IsZero() && filter.Until.IsZero()

	if filter.TraceID != "" {
		return exact && len(filter.ProjectIDs) == 0 && len(filter.Levels) == 0 && len(filter.SpanIDs) == 0
	}

	if len(filter.SpanIDs) > 0 {
		return exact && len(filter.ProjectIDs) == 0 && len(filter.Levels) == 0
	}

	return exact
}

func logView(record logRecord) model.LogRecord {
	return model.LogRecord{
		ID: int64(record.Sequence), ProjectID: record.ProjectID, Timestamp: record.Timestamp, Level: record.Level,
		SeverityNumber: record.SeverityNumber, Body: record.Body, TraceID: record.TraceID, SpanID: record.SpanID,
		Attributes: json.RawMessage(record.Attributes),
	}
}

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

// LogLevels looks for one key per level: a log's key starts with its level's rank.
func (database *Store) LogLevels(context.Context) ([]string, error) {
	var levels []string

	for _, rank := range allRanks {
		found := false
		err := database.logs.Primary(kv.U8(rank)).Limit(1).Keys(database.db, func(kv.Key) (bool, error) {
			found = true

			return false, nil
		})

		if err != nil {
			return nil, err
		}

		if found {
			levels = append(levels, config.LogLevels[rank])
		}
	}

	return levels, nil
}

func (database *Store) CountLogs(_ context.Context, filter model.LogFilter) (int, bool, error) {
	count := 0

	if countByKeys(filter) {
		// One more than the limit tells there are more.
		for _, query := range database.logQueries(filter) {
			found, _, err := query.Count(database.db, model.LogCountLimit+1-count)

			if count += found; err != nil {
				return 0, false, err
			}

			if count > model.LogCountLimit {
				return model.LogCountLimit, true, nil
			}
		}

		return count, false, nil
	}

	err := database.scanLogs(filter, func(logRecord) bool {
		count++

		return count <= model.LogCountLimit
	})

	if count > model.LogCountLimit {
		return model.LogCountLimit, true, err
	}

	return count, false, err
}

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
			database.events.By("span", kv.Text(record.SpanID)).Head(), database.events.By("parent", kv.Text(record.SpanID)).Head(),
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
