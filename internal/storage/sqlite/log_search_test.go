package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	domain "github.com/devforth/exception-monitoring/internal/storage/store"
)

func saveBodies(t *testing.T, database *Store, bodies ...string) {
	t.Helper()

	logs := make([]NewLog, 0, len(bodies))

	for _, body := range bodies {
		logs = append(logs, NewLog{ProjectID: 1, Timestamp: time.Now(), Level: "info", Body: body})
	}

	if err := database.SaveLogs(context.Background(), logs, 0); err != nil {
		t.Fatal(err)
	}
}

func searchBodies(t *testing.T, database *Store, text string) []string {
	t.Helper()

	logs, err := database.ListLogs(context.Background(), LogFilter{Query: text, Limit: 100})

	if err != nil {
		t.Fatal(err)
	}

	var bodies []string

	for _, record := range logs {
		bodies = append(bodies, record.Body)
	}

	return bodies
}

// A search finds the substring anywhere, in any case, newest first, through
// the index or (short text, wildcards) by reading the logs; both agree.
func TestLogSearch(t *testing.T) {
	database := openTestStore(t)
	saveBodies(t, database, "Request timeout after 30s", "user 42 logged in", "TIMEOUT in worker", "100% done", "a_b c")

	for text, want := range map[string][]string{
		"timeout":    {"TIMEOUT in worker", "Request timeout after 30s"},
		"ged i":      {"user 42 logged in"},
		"42":         {"user 42 logged in"}, // too short for the index
		"0%":         {"100% done"},         // a wildcard is searched as text
		"0% d":       {"100% done"},         // also through the index
		"a_b":        {"a_b c"},
		"not there":  nil,
		"timeout af": {"Request timeout after 30s"},
	} {
		if got := searchBodies(t, database, text); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}

	if !database.usesSearchIndex(context.Background(), "timeout") || database.usesSearchIndex(context.Background(), "42") || !database.usesSearchIndex(context.Background(), "0% d") {
		t.Error("the index is used for the wrong searches")
	}

	// Deleted logs stay in the index but are not found; an id used again
	// (the newest log deleted) does not match the old text.
	if _, err := database.DB.Exec(`DELETE FROM logs WHERE body LIKE 'Request%' OR body = 'a_b c'`); err != nil {
		t.Fatal(err)
	}

	saveBodies(t, database, "something else")

	if got := searchBodies(t, database, "timeout"); !slices.Equal(got, []string{"TIMEOUT in worker"}) {
		t.Errorf("after delete: %v", got)
	}

	if got := searchBodies(t, database, "a_b c"); got != nil {
		t.Errorf("a reused id matched the deleted text: %v", got)
	}
}

// Logs from before the index: searched by reading them until the backfill
// is done, deleted meanwhile (retention), then found through it.
func TestLogSearchBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	before, err := openWithMigrations(ctx, path, migrations[:21])

	if err != nil {
		t.Fatal(err)
	}

	var bodies []string

	for number := range 25 {
		bodies = append(bodies, fmt.Sprintf("old log %d", number))
	}

	saveBodies(t, before, bodies...)
	_ = before.Close()

	database, err := Open(ctx, path)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = database.Close() })

	saveBodies(t, database, "new log after the index")

	if database.LogSearchReady(ctx) {
		t.Fatal("ready before the backfill")
	}

	if got := searchBodies(t, database, "old log 1"); len(got) != 11 { // 1 and 10–19
		t.Errorf("before the backfill: %v", got)
	}

	// Retention deletes old logs on both sides of the backfill.
	if _, err := database.IndexLogsForSearch(ctx, 10); err != nil {
		t.Fatal(err)
	}

	if _, err := database.DB.Exec(`DELETE FROM logs WHERE body IN ('old log 3', 'old log 20')`); err != nil {
		t.Fatal(err)
	}

	for done := false; !done; {
		if done, err = database.IndexLogsForSearch(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}

	if !database.LogSearchReady(ctx) || !database.usesSearchIndex(ctx, "old log") {
		t.Fatal("not ready after the backfill")
	}

	if got := searchBodies(t, database, "old log 2"); len(got) != 5 { // 2, 21–24; 20 deleted
		t.Errorf("after the backfill: %v", got)
	}

	if got := searchBodies(t, database, "after the index"); len(got) != 1 {
		t.Errorf("new log: %v", got)
	}
}

func TestLogCountStopsAtTheLimit(t *testing.T) {
	database := openTestStore(t)
	saveBodies(t, database, strings.Split("a b c d e f g", " ")...)

	defer func(limit int) { domain.LogCountLimit = limit }(domain.LogCountLimit)

	domain.LogCountLimit = 5

	if count, more, err := database.CountLogs(context.Background(), LogFilter{}); err != nil || count != 5 || !more {
		t.Errorf("count %d, more %v, err %v", count, more, err)
	}

	if count, more, err := database.CountLogs(context.Background(), LogFilter{Query: "c"}); err != nil || count != 1 || more {
		t.Errorf("filtered: count %d, more %v, err %v", count, more, err)
	}
}

// A log search reads the index newest first and looks up each log by id: no
// scan of logs, no sort, so it stops after the page.
func TestLogSearchPlan(t *testing.T) {
	database := openTestStore(t)
	from, where, args, order := database.logQuery(context.Background(), LogFilter{Query: "timeout", Levels: []string{"error", "warn"}})
	plan := planOf(t, database, `SELECT logs.id`+from+where+order+` LIMIT 20`, args...)

	if !strings.Contains(plan, "SCAN logs_search VIRTUAL TABLE") || !strings.Contains(plan, "SEARCH logs USING INTEGER PRIMARY KEY") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("plan: %s", plan)
	}
}

// Once the index holds more deleted logs than live ones, it is emptied and
// filled again; searches read logs meanwhile.
func TestRefreshLogSearch(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)

	defer func(limit int64) { minStaleLogs = limit }(minStaleLogs)

	minStaleLogs = 1

	var bodies []string

	for number := range 30 {
		bodies = append(bodies, fmt.Sprintf("job %d finished", number))
	}

	saveBodies(t, database, bodies...)

	if _, err := database.DB.Exec(`DELETE FROM logs WHERE id <= 15`); err != nil {
		t.Fatal(err)
	}

	// 15 deleted, 15 left: not yet.
	if refreshed, err := database.RefreshLogSearch(ctx); err != nil || refreshed {
		t.Fatalf("refreshed %v, err %v", refreshed, err)
	}

	if _, err := database.DB.Exec(`DELETE FROM logs WHERE id <= 20`); err != nil {
		t.Fatal(err)
	}

	if refreshed, err := database.RefreshLogSearch(ctx); err != nil || !refreshed || database.LogSearchReady(ctx) {
		t.Fatalf("refreshed %v, err %v", refreshed, err)
	}

	saveBodies(t, database, "job 99 finished")

	if got := searchBodies(t, database, "finished"); len(got) != 11 {
		t.Errorf("while filling: %v", got)
	}

	for done := false; !done; {
		var err error

		if done, err = database.IndexLogsForSearch(ctx, 4); err != nil {
			t.Fatal(err)
		}
	}

	if !database.usesSearchIndex(ctx, "finished") {
		t.Fatal("not ready after filling")
	}

	if got := searchBodies(t, database, "finished"); len(got) != 11 {
		t.Errorf("after filling: %v", got)
	}

	// The index holds the live logs only.
	var indexed int

	if err := database.DB.QueryRow(`SELECT COUNT(*) FROM logs_search WHERE logs_search MATCH ?`, searchMatch("job")).Scan(&indexed); err != nil || indexed != 11 {
		t.Errorf("indexed %d, err %v", indexed, err)
	}
}

func TestSearchMatch(t *testing.T) {
	for text, want := range map[string]string{
		"Timeout":     `"tim" AND "ime" AND "meo" AND "eou" AND "out"`,
		"aaaa":        `"aaa"`,
		`say "hi"`:    `"say" AND "ay " AND "y """ AND " ""h" AND """hi" AND "hi"""`,
		"ab":          "",
		"привіт світ": `"при" AND "рив" AND "иві" AND "віт" AND "іт " AND "т с" AND " св" AND "сві"`,
	} {
		if got := searchMatch(text); got != want {
			t.Errorf("searchMatch(%q) = %s, want %s", text, got, want)
		}
	}

	if pieces := strings.Count(searchMatch("abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ"), " AND "); pieces != maxSearchTrigrams-1 {
		t.Errorf("a long text asks for %d pieces", pieces+1)
	}
}
