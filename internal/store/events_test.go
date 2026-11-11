package store

import (
	"context"
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
	event.Title = strings.Repeat("я", 2000)
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
	})

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
