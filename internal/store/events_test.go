package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "data"))

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeDatabase(t, database) })

	return database
}

var baseTime = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func testEvent(eventID, fingerprint string, receivedAt time.Time) NewEvent {
	return NewEvent{
		ProjectID:   1,
		EventID:     eventID,
		Fingerprint: fingerprint,
		Title:       "Error: boom",
		Level:       "error",
		Timestamp:   receivedAt,
		ReceivedAt:  receivedAt,
		Payload:     []byte(`{"message":"boom"}`),
	}
}

type issueRow struct {
	status     string
	eventCount int
	firstSeen  string
	lastSeen   string
	resolvedAt *string
}

func readIssue(t *testing.T, database *Store, issueID int64) issueRow {
	t.Helper()

	var row issueRow

	err := database.DB.QueryRow(`SELECT status, event_count, first_seen, last_seen, resolved_at FROM issues WHERE id = ?`, issueID).
		Scan(&row.status, &row.eventCount, &row.firstSeen, &row.lastSeen, &row.resolvedAt)

	if err != nil {
		t.Fatal(err)
	}

	return row
}

func save(t *testing.T, database *Store, event NewEvent) SaveResult {
	t.Helper()

	result, err := database.SaveEvent(context.Background(), event)

	if err != nil {
		t.Fatal(err)
	}

	return result
}

func TestEventsGroupIntoIssues(t *testing.T) {
	database := openTestStore(t)

	first := save(t, database, testEvent("e1", "fp-a", baseTime))

	if !first.NewIssue || first.Duplicate || first.Regression {
		t.Fatalf("first event: %+v", first)
	}

	second := save(t, database, testEvent("e2", "fp-a", baseTime.Add(time.Minute)))

	if second.NewIssue || second.IssueID != first.IssueID {
		t.Fatalf("second event must join the issue: %+v", second)
	}

	other := save(t, database, testEvent("e3", "fp-b", baseTime))

	if !other.NewIssue || other.IssueID == first.IssueID {
		t.Fatalf("other fingerprint must create a new issue: %+v", other)
	}

	issue := readIssue(t, database, first.IssueID)

	if issue.eventCount != 2 || issue.status != StatusUnresolved {
		t.Fatalf("issue = %+v", issue)
	}

	if issue.firstSeen != "2026-11-11T10:00:00.000000Z" || issue.lastSeen != "2026-11-11T10:01:00.000000Z" {
		t.Fatalf("first_seen=%s last_seen=%s", issue.firstSeen, issue.lastSeen)
	}
}

func TestSameFingerprintInOtherProjectIsOtherIssue(t *testing.T) {
	database := openTestStore(t)
	backend := save(t, database, testEvent("e1", "fp", baseTime))
	frontendEvent := testEvent("e1", "fp", baseTime)
	frontendEvent.ProjectID = 2
	frontend := save(t, database, frontendEvent)

	if !frontend.NewIssue || frontend.Duplicate || frontend.IssueID == backend.IssueID {
		t.Fatalf("projects must not share issues or event ids: %+v", frontend)
	}
}

func TestDuplicateEventIsIgnored(t *testing.T) {
	database := openTestStore(t)
	first := save(t, database, testEvent("e1", "fp", baseTime))
	retry := save(t, database, testEvent("e1", "fp", baseTime.Add(time.Second)))

	if !retry.Duplicate {
		t.Fatalf("retry: %+v", retry)
	}

	if issue := readIssue(t, database, first.IssueID); issue.eventCount != 1 {
		t.Fatalf("event_count = %d, a retry must not count twice", issue.eventCount)
	}
}

func TestResolvedIssueRegresses(t *testing.T) {
	database := openTestStore(t)
	first := save(t, database, testEvent("e1", "fp", baseTime))

	if err := database.SetIssueStatus(context.Background(), first.IssueID, StatusResolved); err != nil {
		t.Fatal(err)
	}

	if issue := readIssue(t, database, first.IssueID); issue.status != StatusResolved || issue.resolvedAt == nil {
		t.Fatalf("after resolve: %+v", issue)
	}

	again := save(t, database, testEvent("e2", "fp", baseTime.Add(time.Hour)))

	if !again.Regression || again.NewIssue {
		t.Fatalf("want a regression: %+v", again)
	}

	if issue := readIssue(t, database, first.IssueID); issue.status != StatusUnresolved || issue.resolvedAt != nil {
		t.Fatalf("after regression: %+v", issue)
	}

	// Only the first event after resolving is a regression.
	if next := save(t, database, testEvent("e3", "fp", baseTime.Add(2*time.Hour))); next.Regression {
		t.Fatalf("next event: %+v", next)
	}
}

func TestIgnoredIssueStaysIgnored(t *testing.T) {
	database := openTestStore(t)
	first := save(t, database, testEvent("e1", "fp", baseTime))

	if err := database.SetIssueStatus(context.Background(), first.IssueID, StatusIgnored); err != nil {
		t.Fatal(err)
	}

	if again := save(t, database, testEvent("e2", "fp", baseTime.Add(time.Hour))); again.Regression {
		t.Fatalf("ignored issue must not regress: %+v", again)
	}

	if issue := readIssue(t, database, first.IssueID); issue.status != StatusIgnored || issue.eventCount != 2 {
		t.Fatalf("issue = %+v", issue)
	}
}

func TestLateEventDoesNotMoveLastSeenBack(t *testing.T) {
	database := openTestStore(t)
	first := save(t, database, testEvent("e1", "fp", baseTime.Add(time.Hour)))
	save(t, database, testEvent("e2", "fp", baseTime))

	if issue := readIssue(t, database, first.IssueID); issue.lastSeen != "2026-11-11T11:00:00.000000Z" {
		t.Fatalf("last_seen = %s", issue.lastSeen)
	}
}

func TestSetIssueStatusErrors(t *testing.T) {
	database := openTestStore(t)

	if err := database.SetIssueStatus(context.Background(), 999, StatusResolved); err == nil {
		t.Fatal("expected an error for a missing issue")
	}

	issue := save(t, database, testEvent("e1", "fp", baseTime))

	if err := database.SetIssueStatus(context.Background(), issue.IssueID, "deleted"); err == nil {
		t.Fatal("expected an error for an invalid status")
	}
}

func TestLongTitleIsTruncated(t *testing.T) {
	database := openTestStore(t)
	event := testEvent("e1", "fp", baseTime)
	// "é" is two bytes: the limit must count characters, not cut one in half.
	event.Title = strings.Repeat("é", 2000)
	result := save(t, database, event)

	var title string

	if err := database.DB.QueryRow(`SELECT title FROM issues WHERE id = ?`, result.IssueID).Scan(&title); err != nil {
		t.Fatal(err)
	}

	if runes := []rune(title); len(runes) != maxTitleLength || runes[len(runes)-1] != '…' {
		t.Fatalf("title length = %d", len(runes))
	}
}

func TestSaveLogs(t *testing.T) {
	database := openTestStore(t)
	err := database.SaveLogs(context.Background(), []NewLog{
		{ProjectID: 1, Timestamp: baseTime, Level: "info", Body: "started", TraceID: "abc", Attributes: []byte(`{"k":{"type":"string","value":"v"}}`)},
		{ProjectID: 1, Timestamp: baseTime, Level: "warn", Body: "slow"},
	}, 0)

	if err != nil {
		t.Fatal(err)
	}

	var count int

	if err := database.DB.QueryRow(`SELECT COUNT(*) FROM logs WHERE project_id = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}

	var attributes string

	if err := database.DB.QueryRow(`SELECT attributes FROM logs WHERE body = 'slow'`).Scan(&attributes); err != nil {
		t.Fatal(err)
	}

	if count != 2 || attributes != "{}" {
		t.Fatalf("count=%d attributes of a log without them=%s, want {}", count, attributes)
	}
}

// Migration 4 fills span columns of rows stored before it, from their JSON.
func TestSpanMigrationBackfillsExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	before, err := openWithMigrations(ctx, path, migrations[:3])

	if err != nil {
		t.Fatal(err)
	}

	_, err = before.DB.ExecContext(ctx, `
		INSERT INTO issues (id, project_id, fingerprint, title, level, first_seen, last_seen) VALUES (1, 1, 'fp', 'Error', 'error', 'x', 'x');
		INSERT INTO events (project_id, issue_id, event_id, timestamp, received_at, level, trace_id, payload)
		VALUES (1, 1, 'e1', 'x', 'x', 'error', 't1', '{"contexts":{"trace":{"trace_id":"t1","span_id":"handler1","parent_span_id":"request1"}}}');
		INSERT INTO logs (project_id, timestamp, level, body, trace_id, attributes)
		VALUES (1, 'x', 'error', 'log', 't1', '{"sentry.trace.parent_span_id":{"type":"string","value":"request1"}}');`)

	if err != nil {
		t.Fatal(err)
	}

	closeDatabase(t, before)

	database, err := openWithMigrations(ctx, path, migrations)

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	var spanID, parentSpanID, logSpanID string

	if err := database.DB.QueryRow(`SELECT span_id, parent_span_id FROM events`).Scan(&spanID, &parentSpanID); err != nil {
		t.Fatal(err)
	}

	if err := database.DB.QueryRow(`SELECT span_id FROM logs`).Scan(&logSpanID); err != nil {
		t.Fatal(err)
	}

	if spanID != "handler1" || parentSpanID != "request1" || logSpanID != "request1" {
		t.Fatalf("event span=%q parent=%q, log span=%q", spanID, parentSpanID, logSpanID)
	}
}

func TestIssuesSeenInRange(t *testing.T) {
	database := openTestStore(t)

	// Seen before and after the range, but not inside it.
	save(t, database, testEvent("gap-1", "gap", baseTime))
	save(t, database, testEvent("gap-2", "gap", baseTime.Add(10*time.Hour)))
	// Seen inside the range.
	inside := save(t, database, testEvent("in-1", "inside", baseTime.Add(5*time.Hour)))

	issues, err := database.ListIssues(context.Background(), IssueFilter{
		SeenSince: baseTime.Add(4 * time.Hour),
		SeenUntil: baseTime.Add(6 * time.Hour),
		Limit:     10,
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(issues) != 1 || issues[0].ID != inside.IssueID {
		t.Fatalf("got %+v, want only the issue with an event inside the range", issues)
	}

	open, err := database.ListIssues(context.Background(), IssueFilter{SeenSince: baseTime.Add(9 * time.Hour), Limit: 10})

	if err != nil || len(open) != 1 {
		t.Fatalf("since only: %v, %d issues", err, len(open))
	}
}

func TestIssueDateFields(t *testing.T) {
	database := openTestStore(t)

	// "early": first seen at 0h, last seen at 10h. "middle": only at 5h.
	early := save(t, database, testEvent("early-1", "early", baseTime))
	save(t, database, testEvent("early-2", "early", baseTime.Add(10*time.Hour)))
	middle := save(t, database, testEvent("middle-1", "middle", baseTime.Add(5*time.Hour)))

	testCases := []struct {
		name      string
		field     string
		from, to  time.Duration
		wantIssue []int64
	}{
		{"any event at the start", DateAnyEvent, -time.Hour, time.Hour, []int64{early.IssueID}},
		{"first seen at the start", DateFirstSeen, -time.Hour, time.Hour, []int64{early.IssueID}},
		{"last seen at the start", DateLastSeen, -time.Hour, time.Hour, nil},
		{"any event at the end", DateAnyEvent, 9 * time.Hour, 11 * time.Hour, []int64{early.IssueID}},
		{"first seen at the end", DateFirstSeen, 9 * time.Hour, 11 * time.Hour, nil},
		{"last seen at the end", DateLastSeen, 9 * time.Hour, 11 * time.Hour, []int64{early.IssueID}},
		{"first seen in the middle", DateFirstSeen, 4 * time.Hour, 6 * time.Hour, []int64{middle.IssueID}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			issues, err := database.ListIssues(context.Background(), IssueFilter{
				SeenSince: baseTime.Add(testCase.from),
				SeenUntil: baseTime.Add(testCase.to),
				DateField: testCase.field,
				Limit:     10,
			})

			if err != nil {
				t.Fatal(err)
			}

			var got []int64

			for _, issue := range issues {
				got = append(got, issue.ID)
			}

			if fmt.Sprint(got) != fmt.Sprint(testCase.wantIssue) {
				t.Fatalf("got issues %v, want %v", got, testCase.wantIssue)
			}
		})
	}
}

// A message logged in a loop keeps only its newest copies: new logs replace
// the oldest ones as they arrive. Other messages and projects are untouched.
func TestSaveLogsKeepsNewestPerMessage(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()

	for batch := range 3 {
		var logs []NewLog

		for index := range 4 {
			number := batch*4 + index
			logs = append(logs, NewLog{ProjectID: 1, Timestamp: baseTime, Level: "info", Body: fmt.Sprintf("retry %d", number), Fingerprint: "loop"})
		}

		logs = append(logs, NewLog{ProjectID: 2, Timestamp: baseTime, Level: "info", Body: "retry", Fingerprint: "loop"})

		if err := database.SaveLogs(ctx, logs, 5); err != nil {
			t.Fatal(err)
		}
	}

	if err := database.SaveLogs(ctx, []NewLog{{ProjectID: 1, Timestamp: baseTime, Level: "info", Body: "other"}}, 5); err != nil {
		t.Fatal(err)
	}

	var kept []string

	rows, err := database.DB.Query(`SELECT body FROM logs WHERE project_id = 1 AND fingerprint = 'loop' ORDER BY id`)

	if err != nil {
		t.Fatal(err)
	}

	defer rows.Close()

	for rows.Next() {
		var body string

		_ = rows.Scan(&body)
		kept = append(kept, body)
	}

	if fmt.Sprint(kept) != "[retry 7 retry 8 retry 9 retry 10 retry 11]" {
		t.Fatalf("kept %v, want the newest 5", kept)
	}

	var others int

	_ = database.DB.QueryRow(`SELECT COUNT(*) FROM logs WHERE project_id = 2 OR body = 'other'`).Scan(&others)

	if others != 4 {
		t.Fatalf("other project and message: %d logs, want 4", others)
	}
}

// An issue keeps only its newest KeepEvents events, trimmed as each is saved;
// its event count still counts them all.
func TestSaveEventKeepsNewestPerIssue(t *testing.T) {
	database := openTestStore(t)

	var issueID int64

	for index := range 5 {
		event := testEvent(fmt.Sprintf("e%d", index), "fp-busy", baseTime.Add(time.Duration(index)*time.Minute))
		event.KeepEvents = 3
		issueID = save(t, database, event).IssueID
	}

	var kept []string

	rows, err := database.DB.Query(`SELECT event_id FROM events WHERE issue_id = ? ORDER BY received_at`, issueID)

	if err != nil {
		t.Fatal(err)
	}

	defer rows.Close()

	for rows.Next() {
		var eventID string

		_ = rows.Scan(&eventID)
		kept = append(kept, eventID)
	}

	if issue, _ := database.GetIssue(context.Background(), issueID); fmt.Sprint(kept) != "[e2 e3 e4]" || issue.EventCount != 5 {
		t.Fatalf("kept %v, event count %d", kept, issue.EventCount)
	}
}
