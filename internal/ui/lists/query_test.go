package lists

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/platform/config"
)

var testProjects = config.Projects{{ID: 1, Name: "backend"}, {ID: 2, Name: "frontend"}}

func TestCleanListQuery(t *testing.T) {
	raw := url.Values{
		"project":  {"1", "backend", "unknown", "frontend"},
		"level":    {"error", "error", "warn", "fatal"},
		"status":   {"bogus"},
		"sort":     {"first_seen"},
		"from":     {"2026-13-01"},
		"to":       {"2026-11-10"},
		"page":     {"0"},
		"per_page": {"30"},
		"query":    {"  " + strings.Repeat("a", 300) + "  "},
		"trace_id": {"abc"},
		"evil":     {"1"},
	}
	query := CleanListQuery(raw, IssueListParams, testProjects)

	if got := query["project"]; !slices.Equal(got, []string{"backend", "frontend"}) {
		t.Errorf("projects = %v, want backend (by id and name, once) and frontend", got)
	}

	// "warn" is a log level, not an event level.
	if got := query["level"]; !slices.Equal(got, []string{"error", "fatal"}) {
		t.Errorf("levels = %v, want error and fatal once each", got)
	}

	if got := len([]rune(query.Get("query"))); got != maxSearchLength {
		t.Errorf("search has %d characters, want it trimmed and capped at %d", got, maxSearchLength)
	}

	for _, dropped := range []string{"status", "from", "page", "per_page", "trace_id", "evil"} {
		if query.Has(dropped) {
			t.Errorf("%s = %q kept, want it dropped", dropped, query.Get(dropped))
		}
	}

	if query.Get("sort") != "first_seen" || query.Get("to") != "2026-11-10" {
		t.Errorf("valid sort or date dropped: %v", query)
	}

	logs := CleanListQuery(url.Values{"trace_id": {" abc "}, "sort": {"first_seen"}, "status": {"resolved"}, "level": {"warn"}}, LogListParams, testProjects)

	if logs.Get("trace_id") != "abc" || logs.Has("sort") || logs.Has("status") || logs.Get("level") != "warn" {
		t.Errorf("log list query = %v, want trace_id and the log level warn only", logs)
	}
}

func TestParseDateRange(t *testing.T) {
	dates := ParseDateRange(url.Values{"from": {"2026-11-10"}, "to": {"2026-11-01"}})

	if dates.From != "2026-11-01" || dates.To != "2026-11-10" {
		t.Errorf("reversed range = %s..%s, want it swapped", dates.From, dates.To)
	}

	if got := dates.Since.Format("2006-01-02"); got != "2026-11-01" {
		t.Errorf("Since = %s", got)
	}

	// The whole "to" day is included.
	if got := dates.Until.Format("2006-01-02"); got != "2026-11-11" {
		t.Errorf("Until = %s, want the day after to", got)
	}

	if dates.FromMax != "2026-11-10" {
		t.Errorf("FromMax = %s, want to", dates.FromMax)
	}

	invalid := ParseDateRange(url.Values{"from": {"yesterday"}})

	if !invalid.Since.IsZero() || !invalid.Until.IsZero() {
		t.Errorf("invalid date gave bounds %v..%v", invalid.Since, invalid.Until)
	}
}
