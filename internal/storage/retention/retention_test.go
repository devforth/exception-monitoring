package retention_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/sqlite"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/retention"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

type testSetup struct {
	t        *testing.T
	ctx      context.Context
	database *sqlite.Store
	count    int
}

func newSetup(t *testing.T) *testSetup {
	t.Helper()

	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return &testSetup{t: t, ctx: context.Background(), database: database}
}

// event stores an event of the issue with this fingerprint, received age ago.
func (setup *testSetup) event(fingerprint string, age time.Duration, payloadSize int) int64 {
	setup.t.Helper()

	setup.count++
	received := now.Add(-age)

	result, err := setup.database.SaveEvent(setup.ctx, store.NewEvent{
		ProjectID: 1, EventID: fmt.Sprintf("%032d", setup.count), Fingerprint: fingerprint, Title: fingerprint, Level: "error",
		Timestamp: received, ReceivedAt: received,
		Payload: []byte(`{"padding":"` + strings.Repeat("x", payloadSize) + `"}`),
	})

	if err != nil {
		setup.t.Fatal(err)
	}

	return result.IssueID
}

func (setup *testSetup) log(age time.Duration, bodySize int) {
	setup.t.Helper()
	setup.projectLog(1, age, bodySize)
}

func (setup *testSetup) projectLog(projectID int, age time.Duration, bodySize int) {
	setup.t.Helper()

	err := setup.database.SaveLogs(setup.ctx, []store.NewLog{{ProjectID: projectID, Timestamp: now.Add(-age), Level: "info", Body: strings.Repeat("y", bodySize), Attributes: []byte(`{}`)}}, 0)

	if err != nil {
		setup.t.Fatal(err)
	}
}

func (setup *testSetup) clean(settings config.Retention) retention.Result {
	setup.t.Helper()

	return setup.cleanProjects(settings, nil)
}

func (setup *testSetup) cleanProjects(settings config.Retention, projects config.Projects) retention.Result {
	setup.t.Helper()

	cleaner := retention.Cleaner{Store: setup.database, Settings: settings, Projects: projects, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	result, err := cleaner.Clean(setup.ctx)

	if err != nil {
		setup.t.Fatal(err)
	}

	return result
}

func (setup *testSetup) countRows(query string, args ...any) int {
	setup.t.Helper()

	var count int

	if err := setup.database.DB.QueryRow(query, args...).Scan(&count); err != nil {
		setup.t.Fatal(err)
	}

	return count
}

const day = 24 * time.Hour

func TestExpiredEventsAndLogs(t *testing.T) {
	setup := newSetup(t)

	oldIssue := setup.event("old", 50*day, 10)
	setup.event("old", 40*day, 10)
	activeIssue := setup.event("active", 40*day, 10)
	setup.event("active", day, 10)
	setup.log(20*day, 10)
	setup.log(day, 10)

	result := setup.clean(config.Retention{Events: 30 * day, Logs: 14 * day})

	// Old events go, but never the latest one of an issue.
	if result.ExpiredEvents != 2 || result.ExpiredLogs != 1 {
		t.Fatalf("result: %+v", result)
	}

	// Issues stay with their full counts and their latest event.
	for _, issueID := range []int64{oldIssue, activeIssue} {
		issue, err := setup.database.GetIssue(setup.ctx, issueID)

		if err != nil || issue.EventCount != 2 {
			t.Fatalf("issue %d: %+v %v", issueID, issue, err)
		}

		if _, err := setup.database.LatestEvent(setup.ctx, issueID); err != nil {
			t.Errorf("issue %d lost its latest event: %v", issueID, err)
		}
	}

	if kept := setup.countRows(`SELECT COUNT(*) FROM events WHERE issue_id = ? AND received_at >= ?`, oldIssue, now.Add(-41*day).Format("2006-01-02T15:04:05.000000Z")); kept != 1 {
		t.Error("the old issue must keep its newest event, not an older one")
	}

	if logs := setup.countRows(`SELECT COUNT(*) FROM logs`); logs != 1 {
		t.Errorf("logs left: %d", logs)
	}

	// Zero disables a limit.
	setup.event("active", 400*day, 10)

	if result := setup.clean(config.Retention{}); result.ExpiredEvents != 0 {
		t.Errorf("no limits: %+v", result)
	}
}

func TestEventsPerIssue(t *testing.T) {
	setup := newSetup(t)

	var issue int64

	for age := 5; age >= 1; age-- {
		issue = setup.event("busy", time.Duration(age)*time.Hour, 10)
	}

	other := setup.event("quiet", time.Hour, 10)

	if result := setup.clean(config.Retention{EventsPerIssue: 3}); result.TrimmedEvents != 2 {
		t.Fatalf("result: %+v", result)
	}

	// The newest three stay.
	if kept := setup.countRows(`SELECT COUNT(*) FROM events WHERE issue_id = ? AND received_at >= ?`, issue, now.Add(-3*time.Hour).Format("2006-01-02T15:04:05.000000Z")); kept != 3 {
		t.Errorf("newest events kept: %d", kept)
	}

	if events := setup.countRows(`SELECT COUNT(*) FROM events WHERE issue_id = ?`, other); events != 1 {
		t.Errorf("other issue: %d events", events)
	}
}

func TestDatabaseSizeLimit(t *testing.T) {
	setup := newSetup(t)

	// About 4 MB: events and logs, interleaved in time.
	for index := range 200 {
		age := time.Duration(200-index) * time.Minute
		setup.event(fmt.Sprintf("issue-%d", index%5), age, 10_000)
		setup.log(age-30*time.Second, 10_000)
	}

	before, err := setup.database.DatabaseSize(setup.ctx)

	if err != nil {
		t.Fatal(err)
	}

	const limit = 1 << 20

	result := setup.clean(config.Retention{MaxDatabaseBytes: limit})

	if float64(result.SizeBytes) > limit*0.9 || result.SizeBytes >= before || result.OversizeEvents == 0 {
		t.Fatalf("size %d (before %d, limit %d): %+v", result.SizeBytes, before, limit, result)
	}

	// The newest data stays, from both tables.
	if events, logs := setup.countRows(`SELECT COUNT(*) FROM events`), setup.countRows(`SELECT COUNT(*) FROM logs`); events == 0 || logs == 0 {
		t.Fatalf("left %d events, %d logs: %+v", events, logs, result)
	}

	if newest := setup.countRows(`SELECT COUNT(*) FROM events WHERE received_at >= ?`, now.Add(-time.Minute).Format("2006-01-02T15:04:05.000000Z")); newest != 1 {
		t.Error("the newest event must stay")
	}

	// Even over the limit, every issue keeps its latest event.
	if issues := setup.countRows(`SELECT COUNT(DISTINCT issue_id) FROM events`); issues != 5 {
		t.Errorf("issues with an event left: %d of 5", issues)
	}

	// Freed pages go back to the file system, so the file shrinks too.
	if free := setup.countRows(`PRAGMA freelist_count`); free != 0 {
		t.Errorf("%d free pages left in the file", free)
	}
}

func TestExpiredIssues(t *testing.T) {
	setup := newSetup(t)

	forgotten := setup.event("forgotten", 400*day, 10)
	setup.event("forgotten", 370*day, 10)
	recent := setup.event("recent", 400*day, 10)
	setup.event("recent", 300*day, 10)

	result := setup.clean(config.Retention{Issues: 365 * day})

	if result.ExpiredIssues != 1 {
		t.Fatalf("result: %+v", result)
	}

	if _, err := setup.database.GetIssue(setup.ctx, forgotten); err == nil {
		t.Error("an issue not seen for a year must be deleted")
	}

	if events := setup.countRows(`SELECT COUNT(*) FROM events WHERE issue_id = ?`, forgotten); events != 0 {
		t.Errorf("its events must go with it: %d left", events)
	}

	// Seen within the year: stays, old events and all (no event limit set).
	if events := setup.countRows(`SELECT COUNT(*) FROM events WHERE issue_id = ?`, recent); events != 2 {
		t.Errorf("recent issue has %d events, want 2", events)
	}
}

// When only the latest event of each issue is left, the size limit stops
// there instead of deleting them or looping.
func TestSizeLimitKeepsLatestEvents(t *testing.T) {
	setup := newSetup(t)

	for index := range 3 {
		setup.event(fmt.Sprintf("issue-%d", index), time.Hour, 50_000)
	}

	result := setup.clean(config.Retention{MaxDatabaseBytes: 1 << 10})

	if result.OversizeEvents != 0 {
		t.Fatalf("result: %+v", result)
	}

	if events := setup.countRows(`SELECT COUNT(*) FROM events`); events != 3 {
		t.Fatalf("events left: %d of 3", events)
	}
}

// A project's own retention applies to its data only; other projects, and
// data of projects no longer listed, follow the global settings.
func TestProjectRetention(t *testing.T) {
	setup := newSetup(t)

	short := 3 * day
	never := time.Duration(0)
	projects := config.Projects{
		{ID: 1, Name: "backend"},
		{ID: 2, Name: "frontend", Retention: config.RetentionOverride{Logs: &short}},
		{ID: 3, Name: "audit", Retention: config.RetentionOverride{Logs: &never}},
	}

	for _, projectID := range []int{1, 2, 3, 9} {
		setup.projectLog(projectID, 5*day, 10)
		setup.projectLog(projectID, 20*day, 10)
	}

	result := setup.cleanProjects(config.Retention{Logs: 14 * day}, projects)

	if result.ExpiredLogs != 4 {
		t.Fatalf("result: %+v", result)
	}

	for projectID, want := range map[int]int{1: 1, 2: 0, 3: 2, 9: 1} {
		if left := setup.countRows(`SELECT COUNT(*) FROM logs WHERE project_id = ?`, projectID); left != want {
			t.Errorf("project %d: %d logs left, want %d", projectID, left, want)
		}
	}
}

// A lowered limit applies to logs already stored too.
func TestLogsPerMessage(t *testing.T) {
	setup := newSetup(t)

	var logs []store.NewLog

	for index := range 10 {
		logs = append(logs, store.NewLog{ProjectID: 1, Timestamp: now.Add(-time.Duration(index) * time.Minute), Level: "info", Body: "retry", Attributes: []byte(`{}`), Fingerprint: "loop"})
	}

	logs = append(logs, store.NewLog{ProjectID: 1, Timestamp: now, Level: "info", Body: "older format", Attributes: []byte(`{}`)})

	if err := setup.database.SaveLogs(setup.ctx, logs, 0); err != nil {
		t.Fatal(err)
	}

	if result := setup.clean(config.Retention{LogsPerMessage: 3}); result.TrimmedLogs != 7 {
		t.Fatalf("result: %+v", result)
	}

	// Logs stored before fingerprints existed are not one big group.
	if left := setup.countRows(`SELECT COUNT(*) FROM logs`); left != 4 {
		t.Fatalf("%d logs left, want 3 of the message and the one without a fingerprint", left)
	}
}
