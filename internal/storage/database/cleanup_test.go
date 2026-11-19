package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

func clockAt(database *Store, at time.Time) { database.SetClock(func() time.Time { return at }) }

// cleanTo runs the steps down to goal bytes of the counters, without the size checks.
func (database *Store) cleanTo(ctx context.Context, goal int64) ([]CleanupStep, int, error) {
	run := CleanupRun{Status: cleanupRunning, Reached: 1, Goal: goal}
	err := database.advance(ctx, &run)

	return run.Steps, run.Reached, err
}

func writeLogs(t *testing.T, database *Store, level string, count int, body string) {
	t.Helper()

	logs := make([]model.NewLog, 0, count)

	for index := range count {
		logs = append(logs, model.NewLog{
			ProjectID: 1 + index%2, Timestamp: database.now(), Level: level, Body: fmt.Sprintf("%s %d", body, index),
			TraceID: fmt.Sprintf("trace-%d", index), SpanID: fmt.Sprintf("span-%d", index), Fingerprint: fmt.Sprintf("%s-%d", body, index),
		})
	}

	if err := database.SaveLogs(context.Background(), logs, 0); err != nil {
		t.Fatal(err)
	}
}

func writeEvents(t *testing.T, database *Store, fingerprint, level string, count int) {
	t.Helper()

	for index := range count {
		at := database.now().Add(time.Duration(index) * time.Second)

		_, err := database.SaveEvent(context.Background(), model.NewEvent{
			ProjectID: 1, EventID: fmt.Sprintf("%s-%d-%d", fingerprint, at.Unix(), index), Fingerprint: fingerprint, Title: fingerprint,
			Level: level, Timestamp: at, ReceivedAt: at, Payload: []byte(`{"exception":{}}`),
		})

		if err != nil {
			t.Fatal(err)
		}
	}
}

func countLogs(t *testing.T, database *Store, filter model.LogFilter) int {
	t.Helper()

	count, _, err := database.CountLogs(context.Background(), filter)

	if err != nil {
		t.Fatal(err)
	}

	return count
}

func storedEvents(database *Store, fingerprint string) int {
	if issue := storedIssue(database, fingerprint); issue != nil {
		return issue.Stored
	}

	return -1
}

func bytesOf(database *Store, kind, rank byte) int64 {
	var total int64

	for counter, size := range database.db.Counters() {
		decoder := kv.Decode([]byte(counter), 0)

		if decoder.U8() == kind && (rank == 255 || decoder.U8() == rank) {
			total += size
		}
	}

	return total
}

// The month's steps on error logs (others go anyway): old logs and old events
// beyond the newest 20 of their issue go; recent data stays.
func TestCleanupOlderThanAMonth(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "error", 40, "old")
	writeEvents(t, database, "busy", "error", 30)

	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "error", 10, "new")
	writeEvents(t, database, "busy", "error", 5)

	if _, _, err := database.cleanTo(ctx, database.logicalSize()); err != nil {
		t.Fatal(err)
	}

	if logs := countLogs(t, database, model.LogFilter{}); logs != 10 {
		t.Errorf("%d logs left, want the 10 new", logs)
	}

	if logs := countLogs(t, database, model.LogFilter{TraceID: "trace-3"}); logs != 1 {
		t.Errorf("trace of an old log: %d logs, want only the new one", logs)
	}

	// 30 old + 5 new: the old beyond the newest 20 go (15), the new stay.
	if stored := storedEvents(database, "busy"); stored != 20 {
		t.Errorf("busy keeps %d events, want 20", stored)
	}
}

// Deeper: debug logs always, error logs while needed, then events oldest
// first whatever their level (the latest of an issue stays), then the
// latest events, then the issues.
func TestCleanupGoesDeeper(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "old-error", "error", 5)
	clockAt(database, time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "new-warning", "warning", 5)
	writeLogs(t, database, "debug", 30, "debug")
	writeLogs(t, database, "error", 30, "error")
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))

	if _, _, err := database.cleanTo(ctx, database.logicalSize()-bytesOf(database, kindLog, levelRank("debug"))); err != nil {
		t.Fatal(err)
	}

	if debug, errors := countLogs(t, database, model.LogFilter{Levels: []string{"debug"}}), countLogs(t, database, model.LogFilter{Levels: []string{"error"}}); debug != 0 || errors != 30 {
		t.Fatalf("debug %d, error %d", debug, errors)
	}

	// Every log and one event more: the oldest error goes, the newer warnings stay.
	if _, _, err := database.cleanTo(ctx, bytesOf(database, kindEvent, 255)+bytesOf(database, kindIssue, 255)-1); err != nil {
		t.Fatal(err)
	}

	if countLogs(t, database, model.LogFilter{}) != 0 || storedEvents(database, "old-error") != 4 || storedEvents(database, "new-warning") != 5 {
		t.Fatalf("logs %d, old-error %d, new-warning %d", countLogs(t, database, model.LogFilter{}), storedEvents(database, "old-error"), storedEvents(database, "new-warning"))
	}

	// Nothing left to keep: the latest events go, then the issues.
	if _, _, err := database.cleanTo(ctx, 0); err != nil {
		t.Fatal(err)
	}

	if issues, _ := database.CountIssues(ctx, model.IssueFilter{}); issues != 0 || database.logicalSize() != 0 {
		t.Fatalf("issues %d, bytes %d", issues, database.logicalSize())
	}
}

// Events older than a week go before recent ones, whatever their level.
func TestCleanupOlderThanAWeekFirst(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "week-old", "fatal", 5)
	clockAt(database, time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "recent", "debug", 5)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))

	// One event at a time: the week-old ones go first, down to their latest.
	for want := 4; want >= 1; want-- {
		if _, _, err := database.cleanTo(ctx, database.logicalSize()-1); err != nil {
			t.Fatal(err)
		}

		if old, recent := storedEvents(database, "week-old"), storedEvents(database, "recent"); old != want || recent != 5 {
			t.Fatalf("week-old keeps %d, recent %d; want %d and 5", old, recent, want)
		}
	}

	if _, _, err := database.cleanTo(ctx, database.logicalSize()-1); err != nil {
		t.Fatal(err)
	}

	if old, recent := storedEvents(database, "week-old"), storedEvents(database, "recent"); old != 1 || recent != 4 {
		t.Fatalf("then week-old keeps %d, recent %d; want 1 and 4", old, recent)
	}
}

// Resolved and ignored issues keep only their latest event whenever the
// cleanup runs; unresolved ones keep theirs.
func TestCleanupResolvedAndIgnored(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))

	for _, name := range []string{"resolved", "ignored", "open"} {
		writeEvents(t, database, name, "error", 4)
	}

	for name, status := range map[string]string{"resolved": model.StatusResolved, "ignored": model.StatusIgnored} {
		if _, err := database.SetIssuesStatus(ctx, []string{storedIssue(database, name).ID}, status, model.Actor{Source: model.SourceAPI}); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := database.cleanTo(ctx, database.logicalSize()); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{"resolved": 1, "ignored": 1, "open": 4} {
		if stored := storedEvents(database, name); stored != want {
			t.Errorf("%s keeps %d events, want %d", name, stored, want)
		}
	}
}

// At the limit the cleanup brings the database down; below it nothing
// happens.
func TestCleanupThreshold(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "info", 200, "line")

	size, _ := database.db.Size()

	if report, _ := database.Cleanup(ctx, size*2); len(report.Steps) != 0 {
		t.Fatalf("cleaned below the limit: %v", report.Steps)
	}

	report, err := database.Cleanup(ctx, size)

	if err != nil || len(report.Steps) == 0 || report.SizeAfter > size*85/100 {
		t.Fatalf("at the limit: %+v %v", report, err)
	}
}

// Events older than a month keep the error and lose the rest; recent ones
// stay whole, and a second run finds nothing more to thin.
func TestCleanupThinsOldEvents(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	payload := `{"event_id":"x","level":"error","release":"1.2.3","message":"boom","stacktrace":{"frames":[{"function":"report","vars":{"key":"hidden"}}]},` +
		`"exception":{"values":[{"type":"Error","value":"boom","stacktrace":{"frames":[{"function":"handler","context_line":"throw","vars":{"token":"secret"}}]}}]},` +
		`"breadcrumbs":{"values":[{"message":"clicked"}]},"request":{"url":"https://example.com"},"tags":{"a":"b"},"contexts":{"os":{"name":"linux"}}}`

	save := func(fingerprint string, at time.Time) {
		_, err := database.SaveEvent(ctx, model.NewEvent{ProjectID: 1, EventID: fingerprint, Fingerprint: fingerprint, Title: "Error", Level: "error", Timestamp: at, ReceivedAt: at, Payload: []byte(payload)})

		if err != nil {
			t.Fatal(err)
		}
	}

	clockAt(database, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	save("old", database.now())
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	save("recent", database.now())

	before := database.logicalSize()

	if _, err := database.ReduceAging(ctx); err != nil {
		t.Fatal(err)
	}

	old, _ := database.GetEvent(ctx, 1, "old")
	recent, _ := database.GetEvent(ctx, 1, "recent")

	var kept map[string]json.RawMessage

	if err := json.Unmarshal(old.Payload, &kept); err != nil {
		t.Fatal(err)
	}

	if !old.Thinned || recent.Thinned || kept["breadcrumbs"] != nil || kept["request"] != nil || kept["exception"] == nil ||
		strings.Contains(string(old.Payload), "secret") || strings.Contains(string(old.Payload), "hidden") ||
		!strings.Contains(string(old.Payload), `"context_line":"throw"`) || !strings.Contains(string(old.Payload), `"function":"report"`) {
		t.Fatalf("old %v: %s; recent thinned %v", old.Thinned, old.Payload, recent.Thinned)
	}

	thinned := database.logicalSize()

	if thinned >= before {
		t.Fatalf("counters %d, before %d", thinned, before)
	}

	if reduced, err := database.ReduceAging(ctx); err != nil || len(reduced) != 0 || database.logicalSize() != thinned {
		t.Fatalf("thinned again: %d, %+v, %v", database.logicalSize(), reduced, err)
	}
}

// Logs go first; then every issue down to its newest 20 events; only then
// events older than a month down to one per issue.
func TestCleanupOrderOfEvents(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "both", "error", 10)
	writeEvents(t, database, "old", "error", 10)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "both", "error", 3)
	writeEvents(t, database, "recent", "error", 25)
	writeLogs(t, database, "error", 10, "error")

	// A little at a time: no old event goes while logs are left or an issue has more than 20.
	for range 100 {
		if storedEvents(database, "old") == 1 {
			break
		}

		if _, _, err := database.cleanTo(ctx, database.logicalSize()-1); err != nil {
			t.Fatal(err)
		}

		if storedEvents(database, "both") < 13 || storedEvents(database, "old") < 10 {
			if logs, recent := countLogs(t, database, model.LogFilter{}), storedEvents(database, "recent"); logs != 0 || recent != 20 {
				t.Fatalf("old events went while %d logs were left and recent had %d events", logs, recent)
			}
		}
	}

	for fingerprint, want := range map[string]int{"both": 3, "old": 1, "recent": 20} {
		if stored := storedEvents(database, fingerprint); stored != want {
			t.Errorf("then %s keeps %d events, want %d", fingerprint, stored, want)
		}
	}
}

// Logs below error go whenever the cleanup runs, even when less would do;
// error logs only while needed.
func TestCleanupMinorLogsAlways(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "info", 20, "info")
	writeLogs(t, database, "warn", 20, "warn")
	writeLogs(t, database, "error", 20, "error")

	if _, _, err := database.cleanTo(ctx, database.logicalSize()-1); err != nil {
		t.Fatal(err)
	}

	if minor, errors := countLogs(t, database, model.LogFilter{Levels: []string{"info", "warn"}}), countLogs(t, database, model.LogFilter{Levels: []string{"error"}}); minor != 0 || errors != 20 {
		t.Fatalf("info and warn %d, error %d; want 0 and 20", minor, errors)
	}
}

// On disk, the cleanup compacts what it deleted, so the next check sees
// the smaller size and deletes nothing more.
func TestCleanupDoesNotRepeat(t *testing.T) {
	ctx := context.Background()
	database, err := Open(t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = database.Close() })

	// Random text, which does not compress: the files make the size.
	random := rand.New(rand.NewPCG(1, 2))

	for day := range 6 {
		clockAt(database, time.Date(2026, 11, 1+day, 9, 0, 0, 0, time.UTC))

		for range 10 {
			logs := make([]model.NewLog, 0, 500)

			for range 500 {
				logs = append(logs, model.NewLog{ProjectID: 1, Timestamp: database.now(), Level: "info", Body: fmt.Sprintf("%x %x %x %x", random.Uint64(), random.Uint64(), random.Uint64(), random.Uint64())})
			}

			if err := database.SaveLogs(ctx, logs, 0); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Compacted first, then half of that: the cleanup has to delete.
	if err := database.db.CompactRange(nil, nil); err != nil {
		t.Fatal(err)
	}

	size, _ := database.db.Size()
	limit := size / 2
	first, err := database.Cleanup(ctx, limit)

	// Smaller on disk; below the limit is not certain on a database this
	// small, where goleveldb's own files weigh.
	if err != nil || len(first.Steps) == 0 || first.SizeAfter >= first.SizeBefore {
		t.Fatalf("first run: %+v, %v (limit %d)", first, err, limit)
	}

	if runs, err := database.CleanupHistory(ctx, 10); err != nil || len(runs) != 1 || runs[0].Reached < 4 || runs[0].SizeBefore == 0 {
		t.Fatalf("history: %+v, %v", runs, err)
	}

	left := database.logicalSize()

	if second, err := database.Cleanup(ctx, limit); err != nil || len(second.Steps) != 0 || database.logicalSize() != left {
		t.Fatalf("second run deleted again: %+v, %v", second, err)
	}

	checkCollections(t, database)
}

// The package's tests compact at once; TestCompactInboxWhenQuiet sets its own wait.
func init() { quietFor = 0 }

// The inbox holds envelopes as the SDKs sent them, credentials included; once
// processed, their bytes leave the files at a compaction, which waits until
// ingest is quiet (under writes goleveldb would repeat it for as long as they last).
func TestCompactInboxWhenQuiet(t *testing.T) {
	quietFor = 300 * time.Millisecond

	t.Cleanup(func() { quietFor = 0 })

	dir := t.TempDir()
	database, err := Open(dir)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = database.Close() }()

	raw := []byte(`{"request":{"headers":{"Cookie":"sid=7f3a9c2e5b8d1046e9a2c7b4f0d3851a"}}}`)
	entry, err := database.Append(1, time.Now(), raw)

	if err != nil {
		t.Fatal(err)
	}

	if _, err := database.Write([]Envelope{{Sequence: entry.Sequence, InboxSize: entry.Size}}); err != nil {
		t.Fatal(err)
	}

	if err := database.CompactInbox(context.Background()); err != nil || !filesHold(t, dir, raw) {
		t.Fatalf("compacted while ingest was busy: %v", err)
	}

	time.Sleep(quietFor)

	if err := database.CompactInbox(context.Background()); err != nil {
		t.Fatal(err)
	}

	// goleveldb removes replaced files in the background.
	for deadline := time.Now().Add(10 * time.Second); filesHold(t, dir, raw); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a processed envelope is still in the database files")
		}
	}
}

// filesHold says whether a file of the database holds data.
func filesHold(t *testing.T, dir string, data []byte) bool {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(dir, engineName))

	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		// A file may go between the listing and the read.
		content, err := os.ReadFile(filepath.Join(dir, engineName, entry.Name()))

		if err == nil && bytes.Contains(content, data) {
			return true
		}
	}

	return false
}

// The latest events go only for issues not seen for a week; a more recent
// issue keeps its event until the issues themselves go.
func TestCleanupLatestEventsOfWeekOldIssues(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "quiet", "error", 1)
	clockAt(database, time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "recent", "error", 1)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))

	// As much as the quiet issue's event takes: that event goes, the recent
	// issue's stays, and both issues stay.
	quietEvent := database.db.Counter(sizeCounter(kindEvent, levelRank("error"), dayOf(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))))

	if _, _, err := database.cleanTo(ctx, database.logicalSize()-quietEvent); err != nil {
		t.Fatal(err)
	}

	if quiet, recent := storedEvents(database, "quiet"), storedEvents(database, "recent"); quiet != 0 || recent != 1 {
		t.Fatalf("quiet keeps %d, recent %d; want 0 and 1", quiet, recent)
	}
}

// Each step that deleted something is recorded with its number and what it
// deleted per project; Reached is the last step that ran.
func TestCleanupCountsPerStep(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "error", 40, "old")

	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "info", 10, "minor")

	steps, reached, err := database.cleanTo(ctx, database.logicalSize())

	if err != nil {
		t.Fatal(err)
	}

	if reached != 4 || len(steps) != 2 {
		t.Fatalf("reached %d, steps %+v", reached, steps)
	}

	old, minor := steps[0], steps[1]

	if old.Number != 1 || old.Projects[1].Logs != 20 || old.Projects[2].Logs != 20 || old.Freed <= 0 {
		t.Fatalf("step 1: %+v %+v %+v", old, old.Projects[1], old.Projects[2])
	}

	if minor.Number != 4 || minor.Projects[1].Logs != 5 || minor.Projects[2].Logs != 5 {
		t.Fatalf("step 5: %+v", minor)
	}
}

// The history keeps the newest cleanupHistory runs, newest first.
func TestCleanupHistory(t *testing.T) {
	database := openTest(t)

	for index := range cleanupHistory + 5 {
		if err := database.saveCleanup(&CleanupRun{Limit: int64(index)}); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := database.CleanupHistory(context.Background(), cleanupHistory+10)

	if err != nil {
		t.Fatal(err)
	}

	if len(runs) != cleanupHistory || runs[0].Limit != cleanupHistory+4 || runs[len(runs)-1].Limit != 5 {
		t.Fatalf("%d runs, newest %d, oldest %d", len(runs), runs[0].Limit, runs[len(runs)-1].Limit)
	}
}

// runBatch runs one batch of a step from position, as if records were already done in it.
func runBatch(t *testing.T, database *Store, number int, position []byte, records int) ([]byte, bool) {
	t.Helper()

	var finished bool

	cursor := &cleanupCursor{position: position, always: true, records: records}

	err := database.update(func(writer *batchWriter) error {
		var err error

		cursor.batch = writer.batch
		finished, err = database.cleanupSteps()[number-1].batch(writer, cursor)

		return err
	})

	if err != nil {
		t.Fatal(err)
	}

	return cursor.position, finished
}

// A batch judges every issue as it is then: one seen again after the step
// started keeps its events.
func TestCleanupJudgesIssuesAsTheyAre(t *testing.T) {
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "first", "error", 1)
	clockAt(database, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	writeEvents(t, database, "second", "error", 1)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))

	// Step 9: the first batch has room for one issue.
	position, finished := runBatch(t, database, 9, nil, cleanupChunk-1)

	if finished || storedEvents(database, "first") != 0 || storedEvents(database, "second") != 1 {
		t.Fatalf("first batch: finished %v, first %d, second %d", finished, storedEvents(database, "first"), storedEvents(database, "second"))
	}

	writeEvents(t, database, "second", "error", 1)

	if _, finished := runBatch(t, database, 9, position, 0); !finished || storedEvents(database, "second") != 2 {
		t.Fatalf("seen again, second keeps %d events (finished %v)", storedEvents(database, "second"), finished)
	}

	// Step 3: an issue reopened between batches keeps its events.
	writeEvents(t, database, "reopened", "error", 3)

	issue := storedIssue(database, "reopened")

	if _, err := database.SetIssuesStatus(context.Background(), []string{storedIssue(database, "first").ID, issue.ID}, model.StatusResolved, model.Actor{}); err != nil {
		t.Fatal(err)
	}

	position, _ = runBatch(t, database, 3, nil, cleanupChunk)

	if _, err := database.SetIssuesStatus(context.Background(), []string{issue.ID}, model.StatusUnresolved, model.Actor{}); err != nil {
		t.Fatal(err)
	}

	if _, finished := runBatch(t, database, 3, position, 0); !finished || storedEvents(database, "reopened") != 3 {
		t.Fatalf("reopened keeps %d events (finished %v)", storedEvents(database, "reopened"), finished)
	}
}

// A step that is not always stops once it freed what is needed, even inside one issue.
func TestCleanupStopsAtTheGoal(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeEvents(t, database, "busy", "error", 300)

	steps, reached, err := database.cleanTo(ctx, database.logicalSize()-1)

	if err != nil {
		t.Fatal(err)
	}

	// Step 6 deletes one event.
	if reached != 6 || storedEvents(database, "busy") != 299 || len(steps) != 1 || steps[0].Projects[1].Events != 1 {
		t.Fatalf("reached %d, busy keeps %d, steps %+v", reached, storedEvents(database, "busy"), steps)
	}
}

// A run stopped by a restart goes on from its step and position.
func TestCleanupGoesOn(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "info", 10, "old")
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "error", 10, "error")

	// Stopped at step 5 with the goal still ahead; steps 1 to 4 do not run again.
	run := CleanupRun{Status: cleanupRunning, Reached: 5, Goal: database.logicalSize() - bytesOf(database, kindLog, levelRank("error")), Limit: 1}

	if err := database.saveCleanup(&run); err != nil {
		t.Fatal(err)
	}

	done, err := database.Cleanup(ctx, 1<<40)

	if err != nil || done.Status != cleanupDone || done.Reached != 5 || len(done.Steps) != 1 || done.Steps[0].Number != 5 {
		t.Fatalf("run: %+v, %v", done, err)
	}

	if old, errors := countLogs(t, database, model.LogFilter{Levels: []string{"info"}}), countLogs(t, database, model.LogFilter{Levels: []string{"error"}}); old != 10 || errors != 0 {
		t.Fatalf("old info logs %d, error logs %d; want 10 and 0", old, errors)
	}

	if runs, _ := database.CleanupHistory(ctx, 10); len(runs) != 1 || runs[0].Status != cleanupDone {
		t.Fatalf("history: %+v", runs)
	}
}

// Error and fatal logs go together, oldest first.
func TestCleanupErrorAndFatalOldestFirst(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "fatal", 1, "old")
	clockAt(database, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	writeLogs(t, database, "error", 1, "new")

	if _, _, err := database.cleanTo(ctx, database.logicalSize()-1); err != nil {
		t.Fatal(err)
	}

	if fatal, errors := countLogs(t, database, model.LogFilter{Levels: []string{"fatal"}}), countLogs(t, database, model.LogFilter{Levels: []string{"error"}}); fatal != 0 || errors != 1 {
		t.Fatalf("fatal %d, error %d; want the older fatal gone", fatal, errors)
	}
}

// Logs of the ten minutes around the cutoff are not older than a month for sure; they stay.
func TestCleanupMonthBoundary(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	clockAt(database, time.Date(2026, 10, 1, 8, 45, 0, 0, time.UTC))
	writeLogs(t, database, "error", 1, "older")
	clockAt(database, time.Date(2026, 10, 1, 9, 8, 0, 0, time.UTC))
	writeLogs(t, database, "error", 1, "younger")
	clockAt(database, time.Date(2026, 10, 31, 9, 2, 0, 0, time.UTC))

	if _, _, err := database.cleanTo(ctx, database.logicalSize()); err != nil {
		t.Fatal(err)
	}

	if older, younger := countLogs(t, database, model.LogFilter{Query: "older"}), countLogs(t, database, model.LogFilter{Query: "younger"}); older != 0 || younger != 1 {
		t.Fatalf("older %d, younger %d; want 0 and 1", older, younger)
	}
}

// Thinning never makes an event bigger, and keeps <, > and & as they are.
func TestThinNeverGrows(t *testing.T) {
	plain := []byte(`{"exception":{"values":[{"type":"Error","stacktrace":{"frames":[{"function":"<anonymous>","module":"a&b"}]}}]}}`)

	if thinned := thinPayload(plain); string(thinned) != string(plain) {
		t.Fatalf("nothing to thin, got %s", thinned)
	}

	withExtra := []byte(`{"exception":{"values":[{"type":"Error","stacktrace":{"frames":[{"function":"<anonymous>","vars":{"a":1}}]}}]},"breadcrumbs":{"values":[1,2,3]}}`)
	thinned := thinPayload(withExtra)

	if len(thinned) >= len(withExtra) || !strings.Contains(string(thinned), "<anonymous>") || strings.Contains(string(thinned), "vars") {
		t.Fatalf("thinned: %s", thinned)
	}
}

// The store's step counts, shown by the UI, match the steps.
func TestCleanupStepCounts(t *testing.T) {
	steps := openTest(t).cleanupSteps()
	always := 0

	for _, step := range steps {
		if step.always {
			always++
		}
	}

	if len(steps) != model.CleanupSteps || always != model.CleanupAlwaysSteps || !steps[always-1].always {
		t.Fatalf("%d steps, %d always; the store says %d and %d", len(steps), always, model.CleanupSteps, model.CleanupAlwaysSteps)
	}
}
