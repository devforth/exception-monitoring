package database

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

func openTest(t *testing.T) *Store {
	t.Helper()

	database, err := OpenMemory()

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

func saveEvent(t *testing.T, database *Store, fingerprint string, at time.Time) model.SaveResult {
	t.Helper()

	result, err := database.SaveEvent(context.Background(), model.NewEvent{
		ProjectID: 1, EventID: fmt.Sprintf("%032x", at.UnixNano()), Fingerprint: fingerprint, Title: "Error: " + fingerprint,
		Level: "error", Timestamp: at, ReceivedAt: at, Payload: []byte(`{}`), TraceID: "trace-" + fingerprint,
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
// on events, a busy issue stays small in memory and on disk.
func TestIssueStaysSmallWithManyEvents(t *testing.T) {
	database := openTest(t)
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

	for index := range 300 {
		saveEvent(t, database, "busy", start.Add(time.Duration(index)*time.Second))
	}

	issue := storedIssue(database, "busy")

	if issue.Stored != 300 || issue.EventCount != 300 || len(issue.EventDays) != 1 || issue.EventDays[0].Count != 300 {
		t.Fatalf("stored %d, count %d, days %v", issue.Stored, issue.EventCount, issue.EventDays)
	}
}

// "Any event in the range" for a range that covers a day in part reads the
// events; whole days need only the counts.
func TestIssueDateRange(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	morning := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	saveEvent(t, database, "morning", morning)
	saveEvent(t, database, "evening", morning.Add(12*time.Hour))

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
		saveEvent(t, database, "one", start.Add(time.Duration(index)*time.Minute))
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
	result := saveEvent(t, database, "gone", time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC))

	if size, err := database.DatabaseSize(ctx); err != nil || size <= 0 {
		t.Errorf("size %d %v", size, err)
	}

	if _, err := database.SetIssuesStatus(ctx, []string{result.IssueID}, model.StatusResolved, model.Actor{Source: model.SourceAPI}); err != nil {
		t.Fatal(err)
	}

	if history, _ := database.IssueStatusHistory(ctx, result.IssueID); len(history) != 1 {
		t.Fatalf("history %v", history)
	}

	if deleted, err := database.DeleteIssues(ctx, []string{result.IssueID, result.IssueID, "missing0"}); err != nil || deleted != 1 {
		t.Fatalf("deleted %d, %v", deleted, err)
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

// Ping reads the database, so /health fails once it cannot.
func TestPing(t *testing.T) {
	database := openTest(t)

	if err := database.Ping(context.Background()); err != nil {
		t.Fatalf("open store: %v", err)
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if err := database.Ping(context.Background()); err == nil {
		t.Error("a closed store answers the ping")
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

// A project's "no level" stays apart from "the defaults" (nil) once stored.
func TestProjectKeepsNoLevels(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)

	var none []string

	id, err := database.CreateProject(ctx, model.Project{Name: "api", PublicKey: "0123456789abcdef", EventLevels: &none}, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	project, err := database.GetProject(ctx, id)

	if err != nil || project.EventLevels == nil || len(*project.EventLevels) != 0 || project.LogLevels != nil {
		t.Fatalf("stored levels: %v, %v (%v)", project.EventLevels, project.LogLevels, err)
	}

	if id < firstProjectID || id >= firstProjectID+projectIDRange {
		t.Fatalf("id %d is not 8 digits", id)
	}
}

// A database of another format, or one from before the format was recorded, is refused at start.
func TestFormatIsChecked(t *testing.T) {
	dir := t.TempDir()

	for _, test := range []struct {
		change  func(batch *kv.Batch, key kv.Key)
		refusal string
	}{
		{func(*kv.Batch, kv.Key) {}, ""},
		{func(batch *kv.Batch, key kv.Key) { batch.Put(key, []byte("0")) }, "another format: 0"},
		{func(batch *kv.Batch, key kv.Key) { batch.Delete(key) }, "development version"},
	} {
		database, err := Open(dir)

		if err != nil {
			t.Fatal(err)
		}

		writeLogs(t, database, "info", 1, "line")

		if err := database.db.Update(true, func(batch *kv.Batch) error { test.change(batch, kv.Key("format/")); return nil }); err != nil {
			t.Fatal(err)
		}

		_ = database.Close()
		reopened, err := Open(dir)

		if test.refusal == "" && err != nil || test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)) {
			t.Fatalf("reopen: %v, want %q", err, test.refusal)
		}

		if err == nil {
			_ = reopened.Close()
		}

		if err := database.db.Update(true, func(batch *kv.Batch) error { batch.Put(kv.Key("format/"), []byte(storeFormat)); return nil }); err == nil {
			t.Fatal("a closed database accepted a write")
		}

		dir = t.TempDir()
	}
}

// Counts stop one past the limit, by keys or by records alike.
func TestLogCountStopsAtTheLimit(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	limit := model.LogCountLimit
	model.LogCountLimit = 5

	t.Cleanup(func() { model.LogCountLimit = limit })
	writeLogs(t, database, "error", 4, "error")
	writeLogs(t, database, "info", 3, "info")

	for _, test := range []struct {
		filter model.LogFilter
		count  int
		more   bool
	}{
		{model.LogFilter{}, 5, true},
		{model.LogFilter{Levels: []string{"error"}}, 4, false},
		{model.LogFilter{Query: "INFO"}, 3, false},
		{model.LogFilter{ProjectIDs: []int{1, 2}}, 5, true},
	} {
		if count, more, err := database.CountLogs(ctx, test.filter); err != nil || count != test.count || more != test.more {
			t.Errorf("%+v: %d %v %v, want %d %v", test.filter, count, more, err, test.count, test.more)
		}
	}
}

// The search finds the same lines whether the stored bytes can be searched
// first or, for characters JSON escapes, only the decoded line.
func TestLogSearch(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)

	if err := database.SaveLogs(ctx, []model.NewLog{
		{ProjectID: 1, Timestamp: database.now(), Level: "info", Body: `GET /users <id> & "quoted" C:\temp`},
		{ProjectID: 1, Timestamp: database.now(), Level: "info", Body: "nothing here"},
	}, 0); err != nil {
		t.Fatal(err)
	}

	for query, want := range map[string]int{"get /USERS": 1, `"quoted"`: 1, "<id> &": 1, `C:\temp`: 1, "here": 1, "absent": 0, "level": 0} {
		if logs, err := database.ListLogs(ctx, model.LogFilter{Query: query}); err != nil || len(logs) != want {
			t.Errorf("%q: %d logs, want %d (%v)", query, len(logs), want, err)
		}
	}
}

// An outbox message keeps both payloads as they were, either of them empty.
func TestOutboxPayloads(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	now := time.Now()

	for _, payloads := range [][2]string{{`{"Title":"boom"}`, `{"exception":{}}`}, {`{"Count":3}`, ""}} {
		if err := database.EnqueueOutbox(ctx, []string{"conn0001"}, "notification", []byte(payloads[0]), []byte(payloads[1]), now); err != nil {
			t.Fatal(err)
		}
	}

	due, err := database.DueOutbox(ctx, "conn0001", now, 10)

	if err != nil || len(due) != 2 || string(due[0].Payload) != `{"Title":"boom"}` || string(due[0].EventPayload) != `{"exception":{}}` ||
		string(due[1].Payload) != `{"Count":3}` || len(due[1].EventPayload) != 0 {
		t.Fatalf("due: %+v, %v", due, err)
	}

	if count, err := database.CountOutbox(ctx, model.OutboxFilter{Status: model.OutboxPending}); err != nil || count != 2 {
		t.Fatalf("pending: %d, %v", count, err)
	}
}

// Deleting a project removes its issues, events and logs, in several batches,
// and its place in connections; the other projects keep theirs.
func TestDeleteProject(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)

	for _, project := range []model.Project{{ID: 1, Name: "backend", PublicKey: "0123456789abcdef"}, {ID: 2, Name: "frontend", PublicKey: "abcdef0123456789"}} {
		if err := database.SeedProject(ctx, project, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	writeLogs(t, database, "info", 2*deleteChunk+10, "line") // projects 1 and 2 by turns

	for _, project := range []int{1, 2} {
		for index := range 3 {
			if _, err := database.SaveEvent(ctx, model.NewEvent{ProjectID: project, EventID: fmt.Sprintf("%d-%d", project, index), Fingerprint: fmt.Sprint(index),
				Title: "Error", Level: "error", Timestamp: time.Now(), ReceivedAt: time.Now(), Payload: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}
	}

	destination, _ := database.CreateDestination(ctx, model.Destination{Type: "slack", Verified: true}, time.Now())
	connection, err := database.CreateConnection(ctx, model.Connection{Name: "Ops", DestinationID: destination, ProjectIDs: []int{1, 2}, Active: true}, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	if err := database.DeleteProject(ctx, 1); err != nil {
		t.Fatal(err)
	}

	for project, want := range map[int]int{1: 0, 2: deleteChunk + 5} {
		if logs := countLogs(t, database, model.LogFilter{ProjectIDs: []int{project}}); logs != want {
			t.Errorf("project %d keeps %d logs, want %d", project, logs, want)
		}

		if issues, _ := database.CountIssues(ctx, model.IssueFilter{ProjectIDs: []int{project}}); issues != map[int]int{1: 0, 2: 3}[project] {
			t.Errorf("project %d keeps %d issues", project, issues)
		}
	}

	if saved, _ := database.GetConnection(ctx, connection); !slices.Equal(saved.ProjectIDs, []int{2}) {
		t.Errorf("connection projects: %v", saved.ProjectIDs)
	}
}

// Pages kept as the issues go match pages of a full sort, for every order.
func TestIssuePagesMatchAFullSort(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	for index := range 60 {
		at := start.Add(time.Duration(index%7) * time.Minute) // ties, broken by id

		if _, err := database.SaveEvent(ctx, model.NewEvent{ProjectID: 1 + index%2, EventID: fmt.Sprint(index), Fingerprint: fmt.Sprint(index % 30),
			Title: "Error", Level: "error", Timestamp: at, ReceivedAt: at, Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}

	for _, sort := range model.IssueSorts {
		all, err := database.ListIssues(ctx, model.IssueFilter{Sort: sort, Offset: maxKeptIssues})

		if err != nil || len(all) != 0 {
			t.Fatalf("%s: %d issues past the end, %v", sort, len(all), err)
		}

		everything, _ := database.ListIssues(ctx, model.IssueFilter{Sort: sort})

		for offset := 0; offset < 30; offset += 7 {
			page, _ := database.ListIssues(ctx, model.IssueFilter{Sort: sort, Offset: offset, Limit: 7})
			want := everything[offset:min(offset+7, len(everything))]

			if !slices.EqualFunc(page, want, func(a, b model.Issue) bool { return a.ID == b.ID }) {
				t.Errorf("%s from %d: got %v, want %v", sort, offset, page, want)
			}
		}
	}
}
