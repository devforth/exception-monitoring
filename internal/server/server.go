// Package server wires HTTP routes and middleware.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/devforth/exception-monitoring/internal/access"
	"github.com/devforth/exception-monitoring/internal/httpjson"
)

// Pinger reports whether the storage is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Routes is a group of routes (API, UI) registered with a protection wrapper.
type Routes interface {
	Register(router *http.ServeMux, protect func(http.Handler) http.Handler)
}

// Options are the server's dependencies.
type Options struct {
	Logger   *slog.Logger
	Database Pinger
	// Ingest handles SDK envelopes (see package ingest). Open to everyone.
	Ingest http.Handler
	// API adds the admin REST API routes, wrapped with the given protection.
	API Routes
	// UI adds the web interface; it handles its own login, the server adds the IP allowlist.
	UI             Routes
	APIToken       string
	AllowedIPs     []netip.Prefix
	TrustedProxies []netip.Prefix
}

type Server struct {
	Options
}

func New(options Options) *Server {
	return &Server{Options: options}
}

func (server *Server) Handler() http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /health", server.health)
	// SDKs derive this URL from the DSN. No method in the pattern: the ingest
	// handler answers CORS preflight (OPTIONS) itself. {$} means an exact match.
	router.Handle("/api/{projectID}/envelope/{$}", server.Ingest)
	router.Handle("/api/{projectID}/envelope", server.Ingest)

	if server.API != nil {
		server.API.Register(router, server.protectAPI)
	}

	if server.UI != nil {
		server.UI.Register(router, server.protectUI)
	}

	return server.logRequests(router)
}

func (server *Server) protectUI(next http.Handler) http.Handler {
	return access.RequireAllowedIP(server.AllowedIPs, server.TrustedProxies, next)
}

// protectAPI: the IP check runs first, so clients outside the allowlist
// cannot even probe tokens.
func (server *Server) protectAPI(next http.Handler) http.Handler {
	return access.RequireAllowedIP(server.AllowedIPs, server.TrustedProxies, access.RequireToken(server.APIToken, next))
}

func (server *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)

	defer cancel()

	if err := server.Database.Ping(ctx); err != nil {
		server.Logger.Error("health check: database unavailable", "err", err)
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "error": "database unavailable"})

		return
	}

	httpjson.Write(w, http.StatusOK, map[string]string{"status": "ok"})
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
		server.Logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration", time.Since(start),
		)
	})
}
