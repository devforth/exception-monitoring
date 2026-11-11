// Package server wires HTTP routes and middleware.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Pinger reports whether the storage is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Server struct {
	logger   *slog.Logger
	database Pinger
	ingest   http.Handler
}

// New builds the server. ingest handles SDK envelopes (see package ingest).
func New(logger *slog.Logger, database Pinger, ingest http.Handler) *Server {
	return &Server{logger: logger, database: database, ingest: ingest}
}

func (server *Server) Handler() http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /health", server.health)
	// SDKs derive this URL from the DSN. No method in the pattern: the ingest
	// handler answers CORS preflight (OPTIONS) itself. {$} means an exact match.
	router.Handle("/api/{projectID}/envelope/{$}", server.ingest)
	router.Handle("/api/{projectID}/envelope", server.ingest)
	return server.logRequests(router)
}

func (server *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := server.database.Ping(ctx); err != nil {
		server.logger.Error("health check: database unavailable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "error": "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *statusRecorder) WriteHeader(status int) {
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

func (server *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		server.logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration", time.Since(start),
		)
	})
}
