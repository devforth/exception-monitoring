package ingest

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/sentry"
)

// fakeSink records what the handler stores.
type fakeSink struct {
	mutex  sync.Mutex
	events []*sentry.Event
	logs   []sentry.Log
	// projects lists the project name of every stored event or log batch.
	projects []string
	err      error
}

func (sink *fakeSink) StoreEvent(_ context.Context, project config.Project, event *sentry.Event) error {
	sink.mutex.Lock()
	defer sink.mutex.Unlock()

	if sink.err != nil {
		return sink.err
	}

	sink.events = append(sink.events, event)
	sink.projects = append(sink.projects, project.Name)

	return nil
}

func (sink *fakeSink) StoreLogs(_ context.Context, project config.Project, logs []sentry.Log) error {
	sink.mutex.Lock()
	defer sink.mutex.Unlock()

	if sink.err != nil {
		return sink.err
	}

	sink.logs = append(sink.logs, logs...)
	sink.projects = append(sink.projects, project.Name)

	return nil
}

// Same keys and IDs as the fixtures and the sentry-playground DSNs.
func testSettings() *config.Config {
	return &config.Config{
		Projects:           fixtures.Projects(),
		AllowedOrigins:     []string{"*"},
		RateLimitPerMinute: 0,
	}
}

type testServer struct {
	handler *Handler
	router  http.Handler
	sink    *fakeSink
}

func newTestServer(settings *config.Config) *testServer {
	sink := &fakeSink{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), settings, sink)
	router := http.NewServeMux()
	router.Handle("/api/{projectID}/envelope/{$}", handler)

	return &testServer{handler: handler, router: router, sink: sink}
}

func (server *testServer) send(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	server.router.ServeHTTP(response, request)

	return response
}

func newRequest(method, target string, body []byte) *http.Request {
	return httptest.NewRequest(method, target, bytes.NewReader(body))
}

func envelopeURL(projectID, key string) string {
	target := "/api/" + projectID + "/envelope/"

	if key != "" {
		target += "?sentry_version=7&sentry_key=" + key
	}

	return target
}

const simpleEnvelope = "{\"event_id\":\"9ec79c33ec9942ab8353589fcb2e04dc\"}\n{\"type\":\"event\"}\n{\"message\":\"hello\"}\n"

func TestReplayRecordedSDKRequests(t *testing.T) {
	for _, fixture := range fixtures.All(t) {
		name := fixture.Name
		t.Run(name, func(t *testing.T) {
			server := newTestServer(testSettings())
			response := server.send(fixture.HTTP())

			if response.Code != http.StatusOK {
				t.Fatalf("got %d %s", response.Code, response.Body)
			}

			wantProject := "backend"

			if fixture.Browser() {
				wantProject = "frontend"

				if response.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3500" {
					t.Fatal("browser request without CORS header in the response")
				}
			}

			for _, project := range server.sink.projects {
				if project != wantProject {
					t.Fatalf("stored for project %q, want %q", project, wantProject)
				}
			}

			var body map[string]string

			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}

			switch {
			case strings.Contains(name, "-event-"):
				if len(server.sink.events) != 1 || body["id"] != server.sink.events[0].EventID {
					t.Fatalf("events=%d response id=%q", len(server.sink.events), body["id"])
				}
			case strings.HasSuffix(name, "-logs"):
				if len(server.sink.logs) == 0 || len(server.sink.events) != 0 {
					t.Fatalf("logs=%d events=%d", len(server.sink.logs), len(server.sink.events))
				}
			default: // spans and client reports are accepted and dropped
				if len(server.sink.projects) != 0 {
					t.Fatalf("%s must not be stored", name)
				}
			}
		})
	}
}

func TestAuthentication(t *testing.T) {
	testCases := []struct {
		name       string
		request    func() *http.Request
		wantStatus int
	}{
		{"key in query", func() *http.Request {
			return newRequest(http.MethodPost, envelopeURL("1", fixtures.BackendKey), []byte(simpleEnvelope))
		}, http.StatusOK},
		{"key in X-Sentry-Auth", func() *http.Request {
			request := newRequest(http.MethodPost, envelopeURL("1", ""), []byte(simpleEnvelope))
			request.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_client=sentry.python/2.0, sentry_key="+fixtures.BackendKey)

			return request
		}, http.StatusOK},
		{"key in Authorization", func() *http.Request {
			request := newRequest(http.MethodPost, envelopeURL("1", ""), []byte(simpleEnvelope))
			request.Header.Set("Authorization", "sentry sentry_key="+fixtures.BackendKey)

			return request
		}, http.StatusOK},
		{"key in envelope dsn (tunnel)", func() *http.Request {
			body := "{\"dsn\":\"https://" + fixtures.BackendKey + "@errors.example.com/1\"}\n{\"type\":\"event\"}\n{}\n"
			return newRequest(http.MethodPost, envelopeURL("1", ""), []byte(body))
		}, http.StatusOK},
		{"envelope dsn for another project", func() *http.Request {
			body := "{\"dsn\":\"https://" + fixtures.BackendKey + "@errors.example.com/2\"}\n{\"type\":\"event\"}\n{}\n"
			return newRequest(http.MethodPost, envelopeURL("1", ""), []byte(body))
		}, http.StatusUnauthorized},
		{"no key", func() *http.Request {
			return newRequest(http.MethodPost, envelopeURL("1", ""), []byte(simpleEnvelope))
		}, http.StatusUnauthorized},
		{"wrong key", func() *http.Request {
			return newRequest(http.MethodPost, envelopeURL("1", fixtures.BrowserKey), []byte(simpleEnvelope))
		}, http.StatusForbidden},
		{"unknown project", func() *http.Request {
			return newRequest(http.MethodPost, envelopeURL("99", fixtures.BackendKey), []byte(simpleEnvelope))
		}, http.StatusForbidden},
		{"non-numeric project", func() *http.Request {
			return newRequest(http.MethodPost, envelopeURL("abc", fixtures.BackendKey), []byte(simpleEnvelope))
		}, http.StatusNotFound},
		{"wrong method", func() *http.Request {
			return newRequest(http.MethodGet, envelopeURL("1", fixtures.BackendKey), nil)
		}, http.StatusMethodNotAllowed},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServer(testSettings())
			response := server.send(testCase.request())

			if response.Code != testCase.wantStatus {
				t.Fatalf("got %d %s, want %d", response.Code, response.Body, testCase.wantStatus)
			}

			if testCase.wantStatus != http.StatusOK && len(server.sink.projects) != 0 {
				t.Fatal("rejected request must not store anything")
			}
		})
	}
}

func TestCORS(t *testing.T) {
	settings := testSettings()
	settings.AllowedOrigins = []string{"https://app.example.com"}
	server := newTestServer(settings)

	t.Run("allowed origin", func(t *testing.T) {
		request := newRequest(http.MethodPost, envelopeURL("2", fixtures.BrowserKey), []byte(simpleEnvelope))
		request.Header.Set("Origin", "https://App.example.com")
		response := server.send(request)

		if response.Code != http.StatusOK || response.Header().Get("Access-Control-Allow-Origin") != "https://App.example.com" {
			t.Fatalf("got %d, allow-origin %q", response.Code, response.Header().Get("Access-Control-Allow-Origin"))
		}
	})

	t.Run("preflight", func(t *testing.T) {
		request := newRequest(http.MethodOptions, envelopeURL("2", fixtures.BrowserKey), nil)
		request.Header.Set("Origin", "https://app.example.com")
		request.Header.Set("Access-Control-Request-Method", "POST")
		response := server.send(request)

		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Access-Control-Allow-Methods"), "POST") {
			t.Fatalf("got %d, headers %v", response.Code, response.Header())
		}
	})

	t.Run("other origin", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodOptions} {
			request := newRequest(method, envelopeURL("2", fixtures.BrowserKey), []byte(simpleEnvelope))
			request.Header.Set("Origin", "https://evil.example.com")
			response := server.send(request)

			if response.Code != http.StatusForbidden || response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("%s: got %d, allow-origin %q", method, response.Code, response.Header().Get("Access-Control-Allow-Origin"))
			}
		}
	})

	t.Run("backend without origin", func(t *testing.T) {
		response := server.send(newRequest(http.MethodPost, envelopeURL("1", fixtures.BackendKey), []byte(simpleEnvelope)))

		if response.Code != http.StatusOK {
			t.Fatalf("got %d", response.Code)
		}
	})
}

func compress(t *testing.T, encoding string, data []byte) []byte {
	t.Helper()

	var (
		buffer bytes.Buffer
		writer io.WriteCloser
	)

	if encoding == "gzip" {
		writer = gzip.NewWriter(&buffer)
	} else {
		writer = zlib.NewWriter(&buffer)
	}

	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

func TestBody(t *testing.T) {
	testCases := []struct {
		name       string
		encoding   string
		body       func(t *testing.T) []byte
		wantStatus int
	}{
		{"gzip", "gzip", func(t *testing.T) []byte { return compress(t, "gzip", []byte(simpleEnvelope)) }, http.StatusOK},
		{"deflate", "deflate", func(t *testing.T) []byte { return compress(t, "deflate", []byte(simpleEnvelope)) }, http.StatusOK},
		{"broken gzip", "gzip", func(t *testing.T) []byte { return []byte("not gzip") }, http.StatusBadRequest},
		{"unsupported encoding", "br", func(t *testing.T) []byte { return []byte(simpleEnvelope) }, http.StatusUnsupportedMediaType},
		{"too large", "", func(t *testing.T) []byte { return bytes.Repeat([]byte("x"), MaxBodyBytes+1) }, http.StatusRequestEntityTooLarge},
		// A small gzip body that expands past the limit (a "zip bomb").
		{"too large after decompression", "gzip", func(t *testing.T) []byte {
			return compress(t, "gzip", bytes.Repeat([]byte("x"), MaxBodyBytes+1))
		}, http.StatusRequestEntityTooLarge},
		{"invalid envelope", "", func(t *testing.T) []byte { return []byte("not an envelope") }, http.StatusBadRequest},
		{"invalid event json", "", func(t *testing.T) []byte { return []byte("{}\n{\"type\":\"event\"}\n{\"message\":\n") }, http.StatusBadRequest},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServer(testSettings())
			request := newRequest(http.MethodPost, envelopeURL("1", fixtures.BackendKey), testCase.body(t))

			if testCase.encoding != "" {
				request.Header.Set("Content-Encoding", testCase.encoding)
			}

			response := server.send(request)

			if response.Code != testCase.wantStatus {
				t.Fatalf("got %d %s, want %d", response.Code, response.Body, testCase.wantStatus)
			}
		})
	}
}

func TestMalformedItemRejectsWholeEnvelope(t *testing.T) {
	server := newTestServer(testSettings())
	body := "{}\n{\"type\":\"event\"}\n{\"message\":\"ok\"}\n{\"type\":\"log\"}\nnot json\n"
	response := server.send(newRequest(http.MethodPost, envelopeURL("1", fixtures.BackendKey), []byte(body)))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", response.Code)
	}

	if len(server.sink.events) != 0 {
		t.Fatal("the valid event must not be stored when another item is malformed")
	}
}

func TestSinkFailure(t *testing.T) {
	server := newTestServer(testSettings())
	server.sink.err = errors.New("disk full")
	response := server.send(newRequest(http.MethodPost, envelopeURL("1", fixtures.BackendKey), []byte(simpleEnvelope)))

	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "disk full") {
		t.Fatalf("got %d %s", response.Code, response.Body)
	}
}

func TestRateLimit(t *testing.T) {
	settings := testSettings()
	settings.RateLimitPerMinute = 2
	server := newTestServer(settings)
	now := time.Date(2026, 11, 11, 10, 0, 15, 0, time.UTC)
	server.handler.now = func() time.Time { return now }

	send := func(projectID, key string) *httptest.ResponseRecorder {
		return server.send(newRequest(http.MethodPost, envelopeURL(projectID, key), []byte(simpleEnvelope)))
	}

	for range 2 {
		if response := send("1", fixtures.BackendKey); response.Code != http.StatusOK {
			t.Fatalf("got %d before the limit", response.Code)
		}
	}

	response := send("1", fixtures.BackendKey)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", response.Code)
	}

	if response.Header().Get("Retry-After") != "46" || response.Header().Get("X-Sentry-Rate-Limits") != "46::project" {
		t.Fatalf("headers %v", response.Header())
	}

	// The limit is per project.
	if response := send("2", fixtures.BrowserKey); response.Code != http.StatusOK {
		t.Fatalf("other project: got %d", response.Code)
	}
	// Requests with a wrong key don't use up the project's limit.
	if response := send("1", fixtures.BrowserKey); response.Code != http.StatusForbidden {
		t.Fatalf("wrong key: got %d, want 403", response.Code)
	}

	now = now.Add(time.Minute)

	if response := send("1", fixtures.BackendKey); response.Code != http.StatusOK {
		t.Fatalf("next minute: got %d", response.Code)
	}
}
