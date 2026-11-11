package ingest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/store"
)

// Replays every recorded SDK request into a real database, twice.
func TestFixturesEndToEnd(t *testing.T) {
	database, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(logger, testSettings(), StoreSink{Store: database, Logger: logger})
	router := http.NewServeMux()
	router.Handle("/api/{projectID}/envelope/{$}", handler)
	server := &testServer{handler: handler, router: router}

	files, err := filepath.Glob(filepath.Join(fixturesDir, "*.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	eventFixtures := 0
	for _, file := range files {
		if strings.Contains(file, "-event-") {
			eventFixtures++
		}
	}

	// The second pass sends the same event_ids again, like an SDK retry.
	for pass := 1; pass <= 2; pass++ {
		for _, file := range files {
			name := strings.TrimSuffix(filepath.Base(file), ".request.json")
			if response := replayFixture(t, server, name); response.Code != http.StatusOK {
				t.Fatalf("pass %d, %s: got %d %s", pass, name, response.Code, response.Body)
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
