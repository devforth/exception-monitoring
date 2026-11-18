package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

func openTest(t *testing.T) *Store {
	t.Helper()

	database, err := OpenMemory(nil)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if !database.closed.Load() {
			checkCollections(t, database)
			_ = database.Close()
		}
	})

	return database
}

// checkCollections verifies the stored data (Store.Verify).
func checkCollections(t *testing.T, database *Store) {
	t.Helper()

	if problems, err := database.Verify(); err != nil || len(problems) > 0 {
		t.Errorf("verify: %v %v", problems, err)
	}
}

func saveEvent(t *testing.T, database *Store, fingerprint string, at time.Time, keep int) model.SaveResult {
	t.Helper()

	result, err := database.SaveEvent(context.Background(), model.NewEvent{
		ProjectID: 1, EventID: fmt.Sprintf("%032x", at.UnixNano()), Fingerprint: fingerprint, Title: "Error: " + fingerprint,
		Level: "error", Timestamp: at, ReceivedAt: at, Payload: []byte(`{}`), KeepEvents: keep, TraceID: "trace-" + fingerprint,
	})

	if err != nil {
		t.Fatal(err)
	}

	return result
}

func storedIssue(database *Store, fingerprint string) *issueRecord {
	database.issuesMutex.RLock()

	defer database.issuesMutex.RUnlock()

	return database.table.byFingerprint[fingerprintKey(1, fingerprint)]
}

// An issue counts its stored events per day, not per event: with no limit
// on events, a busy issue stays small in memory and on disk. With a limit,
// the oldest go.
func TestIssueStaysSmallWithManyEvents(t *testing.T) {
	database := openTest(t)
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

	for index := range 300 {
		saveEvent(t, database, "busy", start.Add(time.Duration(index)*time.Second), 0)
	}

	issue := storedIssue(database, "busy")

	if issue.Stored != 300 || issue.EventCount != 300 || len(issue.EventDays) != 1 || issue.EventDays[0].Count != 300 {
		t.Fatalf("stored %d, count %d, days %v", issue.Stored, issue.EventCount, issue.EventDays)
	}

	saveEvent(t, database, "busy", start.Add(time.Hour), 3)

	events, err := database.ListEvents(context.Background(), model.EventFilter{IssueID: issue.ID})

	if err != nil || len(events) != 3 || storedIssue(database, "busy").Stored != 3 {
		t.Fatalf("after a trim: %d events, %v", len(events), err)
	}
}

// "Any event in the range" for a range that covers a day in part reads the
// events; whole days need only the counts.
func TestIssueDateRange(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	morning := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	saveEvent(t, database, "morning", morning, 0)
	saveEvent(t, database, "evening", morning.Add(12*time.Hour), 0)

	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

	for name, test := range map[string]struct {
		since, until time.Time
		want         int
	}{
		"the whole day":       {day, day.Add(24 * time.Hour), 2},
		"the morning":         {day, day.Add(12 * time.Hour), 1},
		"between the two":     {morning.Add(time.Hour), morning.Add(11 * time.Hour), 0},
		"the next day":        {day.Add(24 * time.Hour), day.Add(48 * time.Hour), 0},
		"from the evening on": {morning.Add(12 * time.Hour), time.Time{}, 1},
	} {
		count, err := database.CountIssues(ctx, model.IssueFilter{SeenSince: test.since, SeenUntil: test.until})

		if err != nil || count != test.want {
			t.Errorf("%s: %d issues (%v), want %d", name, count, err, test.want)
		}
	}
}

// Lists read newest first and continue before an id, as the API's cursors
// do; logs of every level are merged in order.
func TestListCursorsAndPages(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

	for index := range 5 {
		saveEvent(t, database, "one", start.Add(time.Duration(index)*time.Minute), 0)
	}

	issue := storedIssue(database, "one")
	first, _ := database.ListEvents(ctx, model.EventFilter{IssueID: issue.ID, Limit: 2})
	next, _ := database.ListEvents(ctx, model.EventFilter{IssueID: issue.ID, Limit: 10, BeforeID: first[1].ID})

	if len(first) != 2 || len(next) != 3 || !first[0].ReceivedAt.After(first[1].ReceivedAt) || next[0].ID >= first[1].ID {
		t.Fatalf("pages %v / %v", first, next)
	}

	if byTrace, _ := database.ListEvents(ctx, model.EventFilter{TraceID: "trace-one", Limit: 10}); len(byTrace) != 5 {
		t.Errorf("by trace: %d events", len(byTrace))
	}

	var logs []model.NewLog

	for index := range 7 {
		level := []string{"info", "error"}[index%2]
		logs = append(logs, model.NewLog{ProjectID: 1 + index%2, Timestamp: start, Level: level, Body: fmt.Sprintf("line %d", index)})
	}

	if err := database.SaveLogs(ctx, logs, 0); err != nil {
		t.Fatal(err)
	}

	page, _ := database.ListLogs(ctx, model.LogFilter{Limit: 3, Offset: 2})
	rest, _ := database.ListLogs(ctx, model.LogFilter{Limit: 10, BeforeID: page[2].ID})

	if len(page) != 3 || page[0].Body != "line 4" || len(rest) != 2 || rest[0].Body != "line 1" {
		t.Fatalf("logs %v / %v", page, rest)
	}

	if count, more, _ := database.CountLogs(ctx, model.LogFilter{ProjectIDs: []int{2}, Levels: []string{"error"}}); count != 3 || more {
		t.Errorf("project 2 errors: %d", count)
	}
}

// Deleting an issue removes its events, their index keys and its history;
// only the store's own bookkeeping is left.
func TestDeleteIssue(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	result := saveEvent(t, database, "gone", time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC), 0)

	if size, err := database.DatabaseSize(ctx); err != nil || size <= 0 {
		t.Errorf("size %d %v", size, err)
	}

	if _, err := database.SetIssuesStatus(ctx, []int64{result.IssueID}, model.StatusResolved, model.Actor{Source: model.SourceAPI}); err != nil {
		t.Fatal(err)
	}

	if history, _ := database.IssueStatusHistory(ctx, result.IssueID); len(history) != 1 {
		t.Fatalf("history %v", history)
	}

	if err := database.DeleteIssue(ctx, result.IssueID); err != nil {
		t.Fatal(err)
	}

	for _, check := range []func() (int, error){
		func() (int, error) {
			count, _, err := database.events.Primary().Count(database.db, 0)
			return count, err
		},
		func() (int, error) {
			count, _, err := database.status.Primary().Count(database.db, 0)
			return count, err
		},
		func() (int, error) {
			count, _, err := database.issues.Primary().Count(database.db, 0)
			return count, err
		},
	} {
		if count, err := check(); err != nil || count != 0 {
			t.Errorf("left after the delete: %d (%v)", count, err)
		}
	}

	if counters := database.db.Counters(); len(counters) != 0 {
		t.Errorf("counters left: %v", counters)
	}

	if _, err := database.GetIssue(ctx, result.IssueID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("issue: %v", err)
	}

	checkCollections(t, database)

	if err := database.Close(); err != nil || database.Close() != nil {
		t.Error("a second close fails")
	}
}

// A trim in a later batch skips the events an earlier trim of the same
// batch deleted (the database still shows them until the batch is written).
func TestTrimAcrossBatches(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	event := func(index int) model.NewEvent {
		at := start.Add(time.Duration(index) * time.Minute)

		return model.NewEvent{ProjectID: 1, EventID: fmt.Sprintf("%032x", index), Fingerprint: "trim", Title: "Error", Level: "error", Timestamp: at, ReceivedAt: at, Payload: []byte(`{}`), KeepEvents: 3}
	}

	if _, err := database.Write([]Envelope{{Events: []model.NewEvent{event(1), event(2)}}}); err != nil {
		t.Fatal(err)
	}

	if _, err := database.Write([]Envelope{{Events: []model.NewEvent{event(3), event(4), event(5)}}}); err != nil {
		t.Fatal(err)
	}

	issue := storedIssue(database, "trim")
	events, _ := database.ListEvents(ctx, model.EventFilter{IssueID: issue.ID})

	if len(events) != 3 || issue.Stored != 3 || events[2].EventID != fmt.Sprintf("%032x", 3) {
		t.Fatalf("kept %d events (stored %d): %v", len(events), issue.Stored, events)
	}
}

// Logs of a message logged over and over keep their newest copies, also
// when one batch adds more than that.
func TestRepeatedMessages(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	at := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	repeat := func(from, to int) []model.NewLog {
		var logs []model.NewLog

		for index := from; index < to; index++ {
			logs = append(logs, model.NewLog{ProjectID: 1, Timestamp: at, Level: "warn", Body: fmt.Sprintf("retry %d", index), Fingerprint: "retry"})
		}

		return logs
	}

	for _, batch := range [][]model.NewLog{repeat(0, 3), repeat(3, 12)} {
		if err := database.SaveLogs(ctx, batch, 4); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := database.ListLogs(ctx, model.LogFilter{})

	if err != nil || len(logs) != 4 || logs[0].Body != "retry 11" || logs[3].Body != "retry 8" {
		t.Fatalf("kept %v (%v)", logs, err)
	}
}

// A dated log list reads only the records received around the range: the
// time marks bound the scan, and the result is the same.
func TestLogDatesUseTimeMarks(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

	// Three batches received hours apart, each marked by the writer.
	for index, at := range []time.Time{start, start.Add(3 * time.Hour), start.Add(6 * time.Hour)} {
		database.SetClock(func() time.Time { return at })

		if err := database.SaveLogs(ctx, []model.NewLog{{ProjectID: 1, Timestamp: at, Level: "info", Body: fmt.Sprintf("batch %d", index)}}, 0); err != nil {
			t.Fatal(err)
		}
	}

	bodies := func(filter model.LogFilter) []string {
		t.Helper()

		logs, err := database.ListLogs(ctx, filter)

		if err != nil {
			t.Fatal(err)
		}

		var result []string

		for _, record := range logs {
			result = append(result, record.Body)
		}

		return result
	}

	if got := bodies(model.LogFilter{Since: start.Add(2 * time.Hour), Until: start.Add(4 * time.Hour)}); len(got) != 1 || got[0] != "batch 1" {
		t.Errorf("middle: %v", got)
	}

	if got := bodies(model.LogFilter{Until: start.Add(time.Hour)}); len(got) != 1 || got[0] != "batch 0" {
		t.Errorf("until: %v", got)
	}

	if got := bodies(model.LogFilter{Since: start.Add(5 * time.Hour)}); len(got) != 1 || got[0] != "batch 2" {
		t.Errorf("since: %v", got)
	}

	// The bounds: since the middle, the scan stops before batch 0; until
	// the first hour, it starts after batch 0.
	stop := database.sequenceAt(start.Add(2*time.Hour - markSlack))
	end := database.sequenceAfter(start.Add(time.Hour + markSlack))

	if stop == 0 || end == 0 || stop >= end {
		t.Errorf("marks: stop %d, end %d", stop, end)
	}
}

// The inbox keeps an envelope until the batch that stores its data deletes
// it, and its size follows.
func TestInbox(t *testing.T) {
	database := openTest(t)
	received := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	first, err := database.Append(1, received, []byte("one"))

	if err != nil {
		t.Fatal(err)
	}

	second, _ := database.Append(2, received, []byte("two"))

	if database.InboxBytes() != int64(first.Size+second.Size) {
		t.Errorf("inbox bytes %d", database.InboxBytes())
	}

	pending, err := database.Pending(0, 10, func(sequence uint64) bool { return sequence == first.Sequence })

	if err != nil || len(pending) != 1 || pending[0].Sequence != second.Sequence || string(pending[0].Body) != "two" || pending[0].ProjectID != 2 {
		t.Fatalf("pending %+v %v", pending, err)
	}

	if _, err := database.Write([]Envelope{{Sequence: first.Sequence, InboxSize: first.Size}}); err != nil {
		t.Fatal(err)
	}

	if waiting, _ := database.InInbox(first.Sequence); waiting || database.InboxBytes() != int64(second.Size) {
		t.Errorf("stored entry still waiting: %v, %d bytes", waiting, database.InboxBytes())
	}
}
