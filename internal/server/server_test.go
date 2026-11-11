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
		response := request(t, New(logger, fakeDB{}).Handler(), http.MethodGet, "/health")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok"`) {
			t.Fatalf("got %d %s", response.Code, response.Body)
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("Content-Type = %q", contentType)
		}
	})

	t.Run("database down", func(t *testing.T) {
		response := request(t, New(logger, fakeDB{err: errors.New("boom")}).Handler(), http.MethodGet, "/health")
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("got %d, want 503", response.Code)
		}
		if strings.Contains(response.Body.String(), "boom") {
			t.Fatal("internal error details must not leak to the response")
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		response := request(t, New(logger, fakeDB{}).Handler(), http.MethodPost, "/health")
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("got %d, want 405", response.Code)
		}
	})
}
