package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// planOf is the query plan of query, its steps joined by "; ".
func planOf(t *testing.T, database *Store, query string, args ...any) string {
	t.Helper()

	rows, err := database.DB.Query(`EXPLAIN QUERY PLAN `+query, args...)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	var plan []string

	for rows.Next() {
		var id, parent, unused int

		var detail string

		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}

		plan = append(plan, detail)
	}

	return strings.Join(plan, "; ")
}

// The queries of every page: the issue list as it opens and the unresolved
// count in the menu read an index in order instead of scanning and sorting
// every issue.
func TestListQueriesUseIndexes(t *testing.T) {
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	issues, issueArgs := issueWhere(IssueFilter{Status: StatusUnresolved})

	for _, query := range []struct {
		sql   string
		args  []any
		index string
	}{
		{`SELECT id FROM issues` + issues + ` ORDER BY ` + issueSorts["last_seen"] + ` LIMIT 20`, issueArgs, "issues_by_status_last_seen"},
		{`SELECT COUNT(*) FROM issues` + issues, issueArgs, "issues_by_status_last_seen"},
	} {
		if plan := planOf(t, database, query.sql, query.args...); !strings.Contains(plan, query.index) || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s\nplan: %s", query.sql, plan)
		}
	}
}

// The trace and span lookups (an issue's related logs, logs of a trace, the
// issue of a log) must use their partial indexes, not scan the table.
func TestTraceAndSpanLookupsUseIndexes(t *testing.T) {
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	eventsByTrace, eventArgs := eventWhere(EventFilter{TraceID: "trace"})
	logsByTrace, traceArgs := logWhere(LogFilter{TraceID: "trace"})
	logsBySpan, spanArgs := logWhere(LogFilter{SpanIDs: []string{"a", "b"}})

	for _, query := range []struct {
		sql   string
		args  []any
		index string
	}{
		{`SELECT id FROM events` + eventsByTrace, eventArgs, "events_by_trace"},
		{`SELECT id FROM logs` + logsByTrace, traceArgs, "logs_by_trace"},
		{`SELECT id FROM logs` + logsBySpan, spanArgs, "logs_by_span"},
		{eventsBySpanQuery(2), []any{"a", "b", "a", "b"}, "events_by_span"},
	} {
		if text := planOf(t, database, query.sql, query.args...); !strings.Contains(text, query.index) || strings.Contains(text, "SCAN events") || strings.Contains(text, "SCAN logs") {
			t.Errorf("%s\nplan: %s", query.sql, text)
		}
	}
}
