package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDB struct{ err error }

func (fake fakeDB) Ping(context.Context) error { return fake.err }

func request(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))

	return response
}

func TestHealth(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("ok", func(t *testing.T) {
		response := request(t, New(Options{Logger: logger, Database: fakeDB{}, Ingest: http.NotFoundHandler()}).Handler(), http.MethodGet, "/health")

		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok"`) {
			t.Fatalf("got %d %s", response.Code, response.Body)
		}

		if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("Content-Type = %q", contentType)
		}
	})

	t.Run("database down", func(t *testing.T) {
		response := request(t, New(Options{Logger: logger, Database: fakeDB{err: errors.New("boom")}, Ingest: http.NotFoundHandler()}).Handler(), http.MethodGet, "/health")

		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("got %d, want 503", response.Code)
		}

		if strings.Contains(response.Body.String(), "boom") {
			t.Fatal("internal error details must not leak to the response")
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		response := request(t, New(Options{Logger: logger, Database: fakeDB{}, Ingest: http.NotFoundHandler()}).Handler(), http.MethodPost, "/health")

		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("got %d, want 405", response.Code)
		}
	})
}

func TestEnvelopeRoutes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ingest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "project "+r.PathValue("projectID"))
	})
	handler := New(Options{Logger: logger, Database: fakeDB{}, Ingest: ingest}).Handler()

	for _, path := range []string{"/api/7/envelope/", "/api/7/envelope"} {
		response := request(t, handler, http.MethodPost, path)

		if response.Code != http.StatusOK || response.Body.String() != "project 7" {
			t.Fatalf("%s: got %d %q", path, response.Code, response.Body)
		}
	}

	if response := request(t, handler, http.MethodPost, "/api/7/envelope/extra"); response.Code != http.StatusNotFound {
		t.Fatalf("subpath: got %d, want 404", response.Code)
	}
}
