package ingest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/sentry"
	"github.com/devforth/exception-monitoring/internal/store"
)

// Replays every recorded SDK request into a real database, twice.
func TestFixturesEndToEnd(t *testing.T) {
	database, err := store.Open(context.Background(), t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(logger, testSettings(), StoreSink{Store: database, Logger: logger})
	router := http.NewServeMux()
	router.Handle("/api/{projectID}/envelope/{$}", handler)
	server := &testServer{handler: handler, router: router}

	recorded := fixtures.All(t)
	eventFixtures := 0

	for _, fixture := range recorded {
		if strings.Contains(fixture.Name, "-event-") {
			eventFixtures++
		}
	}

	// The second pass sends the same event_ids again, like an SDK retry.
	for pass := 1; pass <= 2; pass++ {
		for _, fixture := range recorded {
			if response := server.send(fixture.HTTP()); response.Code != http.StatusOK {
				t.Fatalf("pass %d, %s: got %d %s", pass, fixture.Name, response.Code, response.Body)
			}
		}
	}

	count := func(query string) int {
		t.Helper()

		var value int

		if err := database.DB.QueryRow(query).Scan(&value); err != nil {
			t.Fatal(err)
		}

		return value
	}

	if events := count(`SELECT COUNT(*) FROM events`); events != eventFixtures {
		t.Fatalf("events = %d, want %d (duplicates must be ignored)", events, eventFixtures)
	}

	if issues := count(`SELECT COUNT(*) FROM issues`); issues != eventFixtures {
		// Every event fixture is a different error, so each is its own issue.
		t.Fatalf("issues = %d, want %d", issues, eventFixtures)
	}

	if frontend := count(`SELECT COUNT(*) FROM issues WHERE project_id = 2`); frontend != 4 {
		t.Fatalf("frontend issues = %d, want 4", frontend)
	}

	if mismatched := count(`SELECT COUNT(*) FROM issues WHERE event_count != (SELECT COUNT(*) FROM events WHERE issue_id = issues.id)`); mismatched != 0 {
		t.Fatalf("%d issues have event_count out of sync", mismatched)
	}
	// Logs have no ID, so a retried batch is stored again: both passes count.
	if logs := count(`SELECT COUNT(*) FROM logs`); logs == 0 || logs%2 != 0 {
		t.Fatalf("logs = %d", logs)
	}

	if linked := count(`SELECT COUNT(*) FROM events WHERE trace_id != ''`); linked != eventFixtures {
		t.Fatalf("events with trace_id = %d, want all %d", linked, eventFixtures)
	}

	if culprits := count(`SELECT COUNT(*) FROM issues WHERE culprit = ''`); culprits != 0 {
		t.Fatalf("%d issues without a culprit", culprits)
	}
}

type recordingNotifier struct {
	notifications []notify.Notification
}

func (notifier *recordingNotifier) Notify(notification notify.Notification) {
	notifier.notifications = append(notifier.notifications, notification)
}

// New issues and regressions are notified; repeats of a known issue are not.
func TestStoreSinkNotifies(t *testing.T) {
	database, err := store.Open(context.Background(), t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()

	notifier := &recordingNotifier{}
	sink := StoreSink{Store: database, Logger: slog.New(slog.DiscardHandler), Notifier: notifier, PublicURL: "https://errors.example.com"}
	project := fixtures.Projects()[0]

	event := func(id string) *sentry.Event {
		parsed, err := sentry.ParseEvent([]byte(`{"event_id":"`+id+`","exception":{"values":[{"type":"Error","value":"boom"}]}}`), "", time.Now())

		if err != nil {
			t.Fatal(err)
		}

		return parsed
	}

	for _, id := range []string{"00000000000000000000000000000001", "00000000000000000000000000000002"} {
		if err := sink.StoreEvent(context.Background(), project, event(id)); err != nil {
			t.Fatal(err)
		}
	}

	if len(notifier.notifications) != 1 || notifier.notifications[0].Kind != notify.NewIssue {
		t.Fatalf("after a new issue and a repeat: %+v", notifier.notifications)
	}

	first := notifier.notifications[0]

	if first.IssueURL != "https://errors.example.com/issues/"+strconv.FormatInt(first.IssueID, 10) || first.Project != "backend" || first.Title != "Error: boom" || first.Event == nil {
		t.Fatalf("notification: %+v", first)
	}

	if err := database.SetIssueStatus(context.Background(), first.IssueID, store.StatusResolved); err != nil {
		t.Fatal(err)
	}

	if err := sink.StoreEvent(context.Background(), project, event("00000000000000000000000000000003")); err != nil {
		t.Fatal(err)
	}

	if len(notifier.notifications) != 2 || notifier.notifications[1].Kind != notify.Regression {
		t.Fatalf("after the resolved issue happened again: %+v", notifier.notifications)
	}
}
