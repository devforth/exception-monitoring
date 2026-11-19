package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/storetest"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Replays every recorded SDK request into a real database, twice.
func TestFixturesEndToEnd(t *testing.T) {
	database := storetest.Open(t)

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

	if events := countEvents(t, database, nil); events != eventFixtures {
		t.Fatalf("events = %d, want %d (duplicates must be ignored)", events, eventFixtures)
	}

	if issues := countIssues(t, database, store.IssueFilter{}, nil); issues != eventFixtures {
		// Every event fixture is a different error, so each is its own issue.
		t.Fatalf("issues = %d, want %d", issues, eventFixtures)
	}

	if frontend := countIssues(t, database, store.IssueFilter{ProjectIDs: []int{2}}, nil); frontend != 4 {
		t.Fatalf("frontend issues = %d, want 4", frontend)
	}

	mismatched := countIssues(t, database, store.IssueFilter{}, func(issue store.Issue) bool {
		return issue.EventCount != countEvents(t, database, func(event store.EventSummary) bool { return event.IssueID == issue.ID })
	})

	if mismatched != 0 {
		t.Fatalf("%d issues have event_count out of sync", mismatched)
	}
	// Logs have no ID, so a retried batch is stored again: both passes count.
	if logs := countLogs(t, database, store.LogFilter{}); logs == 0 || logs%2 != 0 {
		t.Fatalf("logs = %d", logs)
	}

	if linked := countEvents(t, database, func(event store.EventSummary) bool { return event.TraceID != "" }); linked != eventFixtures {
		t.Fatalf("events with trace_id = %d, want all %d", linked, eventFixtures)
	}

	if culprits := countIssues(t, database, store.IssueFilter{}, func(issue store.Issue) bool { return issue.Culprit == "" }); culprits != 0 {
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
	database := storetest.Open(t)

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

	if first.IssueURL != "https://errors.example.com/issues/"+first.IssueID || first.Project != "backend" || first.Title != "Error: boom" || first.Event == nil {
		t.Fatalf("notification: %+v", first)
	}

	if _, err := database.SetIssuesStatus(context.Background(), []string{first.IssueID}, store.StatusResolved, store.Actor{}); err != nil {
		t.Fatal(err)
	}

	if err := sink.StoreEvent(context.Background(), project, event("00000000000000000000000000000003")); err != nil {
		t.Fatal(err)
	}

	if len(notifier.notifications) != 2 || notifier.notifications[1].Kind != notify.Regression {
		t.Fatalf("after the resolved issue happened again: %+v", notifier.notifications)
	}
}

// A message logged in a loop keeps its newest config.LogsPerMessage copies.
func TestStoreSinkLimitsRepeatedLogs(t *testing.T) {
	database := storetest.Open(t)

	sink := StoreSink{Store: database, Logger: slog.New(slog.DiscardHandler)}
	projects := []config.Project{{ID: 1, Name: "backend"}, {ID: 2, Name: "frontend"}}

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

	for projectID, want := range map[int]int{1: config.LogsPerMessage, 2: config.LogsPerMessage} {
		if count := countLogs(t, database, store.LogFilter{ProjectIDs: []int{projectID}}); count != want {
			t.Errorf("project %d: %d logs, want %d", projectID, count, want)
		}
	}
}

// Events and logs of levels a project does not record are dropped: not
// stored and no notification.
func TestStoreSinkRecordsChosenLevels(t *testing.T) {
	database := storetest.Open(t)

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

	// error, fatal and an event without a level (an error) of each project.
	if events := countEvents(t, database, nil); events != 6 {
		t.Errorf("%d events stored, want 6", events)
	}

	if notified := len(notifier.notifications); notified != 6 {
		t.Errorf("%d notifications, want 6: a dropped event must not notify", notified)
	}

	if logs := countLogs(t, database, store.LogFilter{ProjectIDs: []int{1}}); logs != 3 {
		t.Errorf("backend: %d logs, want every level", logs)
	}

	if logs := countLogs(t, database, store.LogFilter{ProjectIDs: []int{2}, Levels: []string{"warn"}}); logs != 1 || countLogs(t, database, store.LogFilter{ProjectIDs: []int{2}}) != 1 {
		t.Error("frontend records only warn logs")
	}
}

// Counts through the store, so they hold for every implementation.

func countEvents(t *testing.T, database store.Store, keep func(store.EventSummary) bool) int {
	t.Helper()

	events, err := database.ListEvents(context.Background(), store.EventFilter{Limit: 1 << 20})

	if err != nil {
		t.Fatal(err)
	}

	count := 0

	for _, event := range events {
		if keep == nil || keep(event) {
			count++
		}
	}

	return count
}

func countIssues(t *testing.T, database store.Store, filter store.IssueFilter, keep func(store.Issue) bool) int {
	t.Helper()

	filter.Limit = 1 << 20
	issues, err := database.ListIssues(context.Background(), filter)

	if err != nil {
		t.Fatal(err)
	}

	count := 0

	for _, issue := range issues {
		if keep == nil || keep(issue) {
			count++
		}
	}

	return count
}

func countLogs(t *testing.T, database store.Store, filter store.LogFilter) int {
	t.Helper()

	count, _, err := database.CountLogs(context.Background(), filter)

	if err != nil {
		t.Fatal(err)
	}

	return count
}
