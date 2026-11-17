package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/sqlite"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Replays every recorded SDK request into a real database, twice.
func TestFixturesEndToEnd(t *testing.T) {
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(logger, testSettings(), fixtures.Projects(), StoreSink{Store: database, Logger: logger})
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
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))

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

	if err := database.SetIssueStatus(context.Background(), first.IssueID, store.StatusResolved, store.Actor{}); err != nil {
		t.Fatal(err)
	}

	if err := sink.StoreEvent(context.Background(), project, event("00000000000000000000000000000003")); err != nil {
		t.Fatal(err)
	}

	if len(notifier.notifications) != 2 || notifier.notifications[1].Kind != notify.Regression {
		t.Fatalf("after the resolved issue happened again: %+v", notifier.notifications)
	}
}

// A message logged in a loop keeps its newest RETENTION_LOGS_PER_MESSAGE
// copies; a project can set its own number.
func TestStoreSinkLimitsRepeatedLogs(t *testing.T) {
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = database.Close() }()

	sink := StoreSink{Store: database, Logger: slog.New(slog.DiscardHandler), Retention: config.Retention{LogsPerMessage: 50}}
	three := 3
	projects := []config.Project{{ID: 1, Name: "backend"}, {ID: 2, Name: "frontend", Retention: config.RetentionOverride{LogsPerMessage: &three}}}

	for _, project := range projects {
		for batch := range 4 {
			var logs []sentry.Log

			for index := range 25 {
				logs = append(logs, sentry.Log{Level: "warn", Body: fmt.Sprintf("retry %d of order %d", index, batch)})
			}

			if err := sink.StoreLogs(context.Background(), project, logs); err != nil {
				t.Fatal(err)
			}
		}
	}

	for projectID, want := range map[int]int{1: 50, 2: 3} {
		var count int

		if err := database.DB.QueryRow(`SELECT COUNT(*) FROM logs WHERE project_id = ?`, projectID).Scan(&count); err != nil || count != want {
			t.Errorf("project %d: %d logs (%v), want %d", projectID, count, err, want)
		}
	}
}

// Events and logs of levels a project does not record are dropped: not
// stored and no notification.
func TestStoreSinkRecordsChosenLevels(t *testing.T) {
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = database.Close() }()

	notifier := &recordingNotifier{}
	sink := StoreSink{Store: database, Logger: slog.New(slog.DiscardHandler), Notifier: notifier,
		Recording: config.Recording{EventLevels: config.DefaultRecordEventLevels}}
	warnOnly := []string{"warn"}
	projects := []config.Project{{ID: 1, Name: "backend"}, {ID: 2, Name: "frontend", LogLevels: &warnOnly}}

	for _, project := range projects {
		for index, level := range []string{"error", "warning", "info", "fatal", ""} {
			event := &sentry.Event{EventID: fmt.Sprintf("%031d%d", project.ID, index), Level: level, Message: sentry.Message("message " + level)}

			if err := sink.StoreEvent(context.Background(), project, event); err != nil {
				t.Fatal(err)
			}
		}

		logs := []sentry.Log{{Level: "info", Body: "a"}, {Level: "warn", Body: "b"}, {Level: "error", Body: "c"}}

		if err := sink.StoreLogs(context.Background(), project, logs); err != nil {
			t.Fatal(err)
		}
	}

	count := func(query string, args ...any) int {
		var value int

		if err := database.DB.QueryRow(query, args...).Scan(&value); err != nil {
			t.Fatal(err)
		}

		return value
	}

	// error, fatal and an event without a level (an error) of each project.
	if events := count(`SELECT COUNT(*) FROM events`); events != 6 {
		t.Errorf("%d events stored, want 6", events)
	}

	if notified := len(notifier.notifications); notified != 6 {
		t.Errorf("%d notifications, want 6: a dropped event must not notify", notified)
	}

	if logs := count(`SELECT COUNT(*) FROM logs WHERE project_id = 1`); logs != 3 {
		t.Errorf("backend: %d logs, want every level", logs)
	}

	if logs := count(`SELECT COUNT(*) FROM logs WHERE project_id = 2 AND level = 'warn'`); logs != 1 || count(`SELECT COUNT(*) FROM logs WHERE project_id = 2`) != 1 {
		t.Error("frontend records only warn logs")
	}
}
