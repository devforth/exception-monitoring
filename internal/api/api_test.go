// Tests run the whole server (ingest + API) against a real database filled
// by replaying the recorded SDK requests.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/api"
	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/ingest"
	"github.com/devforth/exception-monitoring/internal/server"
	"github.com/devforth/exception-monitoring/internal/store"
)

const (
	token       = "0123456789abcdef0123456789abcdef"
	fixturesDir = "../../testdata/envelopes"
)

type testServer struct {
	t       *testing.T
	handler http.Handler
}

func newTestServer(t *testing.T, allowedIPs ...string) *testServer {
	t.Helper()
	database, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	settings := &config.Config{
		Projects: []config.Project{
			{ID: 1, Name: "backend", PublicKey: "backendkey0000000000000000000001"},
			{ID: 2, Name: "frontend", PublicKey: "browserkey0000000000000000000002"},
		},
		AllowedOrigins: []string{"*"},
	}
	var allowed []netip.Prefix
	for _, value := range allowedIPs {
		allowed = append(allowed, netip.MustParsePrefix(value))
	}
	handler := server.New(server.Options{
		Logger:     logger,
		Database:   database,
		Ingest:     ingest.New(logger, settings, ingest.StoreSink{Store: database, Logger: logger}),
		API:        api.New(logger, database, settings.Projects),
		APIToken:   token,
		AllowedIPs: allowed,
	}).Handler()
	return &testServer{t: t, handler: handler}
}

// ingestFixtures sends every recorded SDK request to the server.
func (server *testServer) ingestFixtures() {
	server.t.Helper()
	files, err := filepath.Glob(filepath.Join(fixturesDir, "*.request.json"))
	if err != nil || len(files) == 0 {
		server.t.Fatalf("no fixtures: %v", err)
	}
	for _, file := range files {
		var recorded struct {
			Method, Path string
			Query        map[string]string
		}
		meta, _ := os.ReadFile(file)
		if err := json.Unmarshal(meta, &recorded); err != nil {
			server.t.Fatal(err)
		}
		body, err := os.ReadFile(strings.TrimSuffix(file, ".request.json") + ".envelope")
		if err != nil {
			server.t.Fatal(err)
		}
		query := url.Values{}
		for key, value := range recorded.Query {
			query.Set(key, value)
		}
		request := httptest.NewRequest(recorded.Method, recorded.Path+"?"+query.Encode(), bytes.NewReader(body))
		response := httptest.NewRecorder()
		server.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			server.t.Fatalf("%s: ingest got %d %s", file, response.Code, response.Body)
		}
	}
}

func (server *testServer) do(method, target, body string) *httptest.ResponseRecorder {
	server.t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.handler.ServeHTTP(response, request)
	return response
}

// get requests target and decodes the JSON response into result, failing on a non-200 status.
func (server *testServer) get(target string, result any) {
	server.t.Helper()
	response := server.do(http.MethodGet, target, "")
	if response.Code != http.StatusOK {
		server.t.Fatalf("GET %s: %d %s", target, response.Code, response.Body)
	}
	if err := json.Unmarshal(response.Body.Bytes(), result); err != nil {
		server.t.Fatalf("GET %s: %v", target, err)
	}
}

type issue struct {
	ID         int64  `json:"id"`
	ProjectID  int    `json:"project_id"`
	Project    string `json:"project"`
	Title      string `json:"title"`
	Culprit    string `json:"culprit"`
	Status     string `json:"status"`
	EventCount int    `json:"event_count"`
}

type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func TestAccessControl(t *testing.T) {
	server := newTestServer(t, "10.0.0.0/8")

	request := func(remote, path string, withToken bool) int {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = remote
		if withToken {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.handler.ServeHTTP(response, request)
		return response.Code
	}

	if code := request("10.1.1.1:1", "/api/v1/issues", true); code != http.StatusOK {
		t.Fatalf("allowed ip with token: %d", code)
	}
	if code := request("10.1.1.1:1", "/api/v1/issues", false); code != http.StatusUnauthorized {
		t.Fatalf("allowed ip without token: %d", code)
	}
	if code := request("203.0.113.7:1", "/api/v1/issues", true); code != http.StatusForbidden {
		t.Fatalf("other ip with token: %d", code)
	}
	// Health and ingest are not behind the allowlist.
	if code := request("203.0.113.7:1", "/health", false); code != http.StatusOK {
		t.Fatalf("health from other ip: %d", code)
	}
	if code := request("203.0.113.7:1", "/api/1/envelope/", false); code == http.StatusForbidden {
		t.Fatal("ingest must not be behind the IP allowlist")
	}
}

func TestProjects(t *testing.T) {
	server := newTestServer(t)
	server.ingestFixtures()
	var result struct {
		Items []struct {
			ID               int    `json:"id"`
			Name             string `json:"name"`
			UnresolvedIssues int    `json:"unresolved_issues"`
		} `json:"items"`
	}
	server.get("/api/v1/projects", &result)
	if len(result.Items) != 2 || result.Items[0].Name != "backend" || result.Items[0].UnresolvedIssues != 5 || result.Items[1].UnresolvedIssues != 4 {
		t.Fatalf("projects = %+v", result.Items)
	}
}

func TestListIssues(t *testing.T) {
	server := newTestServer(t)
	server.ingestFixtures()

	var all page[issue]
	server.get("/api/v1/issues", &all)
	if len(all.Items) != 9 || all.NextCursor != nil {
		t.Fatalf("got %d issues, next %v", len(all.Items), all.NextCursor)
	}

	var frontend page[issue]
	server.get("/api/v1/issues?project=frontend", &frontend)
	for _, item := range frontend.Items {
		if item.ProjectID != 2 || item.Project != "frontend" {
			t.Fatalf("project filter leaked %+v", item)
		}
	}
	var byID page[issue]
	server.get("/api/v1/issues?project=2", &byID)
	if len(frontend.Items) != 4 || len(byID.Items) != 4 {
		t.Fatalf("frontend by name=%d by id=%d, want 4", len(frontend.Items), len(byID.Items))
	}

	var search page[issue]
	server.get("/api/v1/issues?query=referenceerror", &search)
	if len(search.Items) != 1 || !strings.HasPrefix(search.Items[0].Title, "ReferenceError") {
		t.Fatalf("search = %+v", search.Items)
	}
	// LIKE wildcards in user input are literal.
	var wildcard page[issue]
	server.get("/api/v1/issues?query=%25", &wildcard)
	if len(wildcard.Items) != 0 {
		t.Fatalf("'%%' must not match everything, got %d", len(wildcard.Items))
	}

	// Walk all pages two at a time: every issue exactly once.
	seen := map[int64]bool{}
	target := "/api/v1/issues?limit=2&sort=first_seen"
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination does not end")
		}
		var current page[issue]
		server.get(target, &current)
		for _, item := range current.Items {
			if seen[item.ID] {
				t.Fatalf("issue %d on two pages", item.ID)
			}
			seen[item.ID] = true
		}
		if current.NextCursor == nil {
			break
		}
		target = "/api/v1/issues?limit=2&sort=first_seen&cursor=" + *current.NextCursor
	}
	if len(seen) != 9 {
		t.Fatalf("pages covered %d issues, want 9", len(seen))
	}

	for _, bad := range []string{"?status=open", "?sort=title", "?limit=0", "?limit=500", "?project=nope", "?cursor=garbage"} {
		if response := server.do(http.MethodGet, "/api/v1/issues"+bad, ""); response.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, response.Code)
		}
	}
}

func TestIssueLifecycle(t *testing.T) {
	server := newTestServer(t)
	server.ingestFixtures()

	var list page[issue]
	server.get("/api/v1/issues?query=Failed+to+load+orders", &list)
	if len(list.Items) != 1 {
		t.Fatalf("got %d issues", len(list.Items))
	}
	target := "/api/v1/issues/" + itoa(list.Items[0].ID)

	response := server.do(http.MethodPatch, target, `{"status":"resolved"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"resolved"`) {
		t.Fatalf("resolve: %d %s", response.Code, response.Body)
	}
	var unresolved page[issue]
	server.get("/api/v1/issues", &unresolved)
	if len(unresolved.Items) != 8 {
		t.Fatalf("default list must hide resolved issues, got %d", len(unresolved.Items))
	}
	var resolved page[issue]
	server.get("/api/v1/issues?status=resolved", &resolved)
	if len(resolved.Items) != 1 {
		t.Fatalf("resolved list: %d", len(resolved.Items))
	}

	for _, body := range []string{`{"status":"closed"}`, `not json`, `{"status":"resolved","extra":1}`} {
		if response := server.do(http.MethodPatch, target, body); response.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s: got %d, want 400", body, response.Code)
		}
	}

	if response := server.do(http.MethodDelete, target, ""); response.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", response.Code)
	}
	for _, path := range []string{target, target + "/events", target + "/events/latest"} {
		if response := server.do(http.MethodGet, path, ""); response.Code != http.StatusNotFound {
			t.Errorf("GET %s after delete: %d, want 404", path, response.Code)
		}
	}
	if response := server.do(http.MethodPatch, "/api/v1/issues/999", `{"status":"resolved"}`); response.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing issue: %d", response.Code)
	}
	if response := server.do(http.MethodGet, "/api/v1/issues/abc", ""); response.Code != http.StatusNotFound {
		t.Fatalf("GET non-numeric id: %d", response.Code)
	}
}

type eventSummary struct {
	EventID string `json:"event_id"`
	IssueID int64  `json:"issue_id"`
	TraceID string `json:"trace_id"`
}

func TestEvents(t *testing.T) {
	server := newTestServer(t)
	server.ingestFixtures()

	var list page[issue]
	server.get("/api/v1/issues?query=ReferenceError", &list)
	issueID := list.Items[0].ID

	var events page[eventSummary]
	server.get("/api/v1/issues/"+itoa(issueID)+"/events", &events)
	if len(events.Items) != 1 || events.Items[0].IssueID != issueID {
		t.Fatalf("events = %+v", events.Items)
	}

	var latest struct {
		eventSummary
		Payload struct {
			Exception struct {
				Values []struct{ Type string } `json:"values"`
			} `json:"exception"`
		} `json:"payload"`
	}
	server.get("/api/v1/issues/"+itoa(issueID)+"/events/latest", &latest)
	if latest.EventID != events.Items[0].EventID || latest.Payload.Exception.Values[0].Type != "ReferenceError" {
		t.Fatalf("latest = %+v", latest)
	}

	// Lookup by event_id, also in UUID form with dashes.
	id := latest.EventID
	dashed := id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
	var byID eventSummary
	server.get("/api/v1/projects/2/events/"+dashed, &byID)
	if byID.EventID != id {
		t.Fatalf("by id = %+v", byID)
	}
	// Event IDs are per project.
	if response := server.do(http.MethodGet, "/api/v1/projects/1/events/"+id, ""); response.Code != http.StatusNotFound {
		t.Fatalf("event from another project: %d", response.Code)
	}
}

type logRecord struct {
	ID         int64           `json:"id"`
	Level      string          `json:"level"`
	Body       string          `json:"body"`
	TraceID    string          `json:"trace_id"`
	Attributes json.RawMessage `json:"attributes"`
}

func TestLogsAndTraces(t *testing.T) {
	server := newTestServer(t)
	server.ingestFixtures()

	var all page[logRecord]
	server.get("/api/v1/logs?limit=200", &all)
	if len(all.Items) == 0 {
		t.Fatal("no logs")
	}
	if !bytes.HasPrefix(all.Items[0].Attributes, []byte("{")) {
		t.Fatalf("attributes must be a JSON object, got %s", all.Items[0].Attributes)
	}

	var warnings page[logRecord]
	server.get("/api/v1/logs?level=warn&project=backend", &warnings)
	for _, record := range warnings.Items {
		if record.Level != "warn" {
			t.Fatalf("level filter leaked %+v", record)
		}
	}
	var pino page[logRecord]
	server.get("/api/v1/logs?query=pino", &pino)
	if len(pino.Items) == 0 || len(pino.Items) >= len(all.Items) {
		t.Fatalf("query matched %d of %d", len(pino.Items), len(all.Items))
	}

	// Pages two at a time cover all logs.
	total := 0
	target := "/api/v1/logs?limit=2"
	for {
		var current page[logRecord]
		server.get(target, &current)
		total += len(current.Items)
		if current.NextCursor == nil {
			break
		}
		target = "/api/v1/logs?limit=2&cursor=" + *current.NextCursor
	}
	if total != len(all.Items) {
		t.Fatalf("pages covered %d logs, want %d", total, len(all.Items))
	}

	var future page[logRecord]
	server.get("/api/v1/logs?since=2100-01-01T00:00:00Z", &future)
	if len(future.Items) != 0 {
		t.Fatalf("since filter: %d", len(future.Items))
	}
	if response := server.do(http.MethodGet, "/api/v1/logs?since=yesterday", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("bad since: %d", response.Code)
	}

	// The browser fixtures share a trace between an error and logs.
	var withTrace page[logRecord]
	server.get("/api/v1/logs?project=frontend", &withTrace)
	traceID := ""
	for _, record := range withTrace.Items {
		if record.TraceID != "" {
			traceID = record.TraceID
		}
	}
	if traceID == "" {
		t.Skip("no frontend log with a trace id in fixtures")
	}
	var trace struct {
		TraceID string         `json:"trace_id"`
		Events  []eventSummary `json:"events"`
		Logs    []logRecord    `json:"logs"`
	}
	server.get("/api/v1/traces/"+traceID, &trace)
	if len(trace.Logs) == 0 {
		t.Fatalf("trace %s has no logs", traceID)
	}
	for _, record := range trace.Logs {
		if record.TraceID != traceID {
			t.Fatalf("trace leaked %+v", record)
		}
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
