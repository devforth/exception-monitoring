// Package api is the read/manage REST API for issues, events and logs:
// /api/v1/... It is used by the UI and is shaped so that an MCP server can map
// its tools onto it one to one (list projects, search issues, get the latest
// event, resolve, search logs, follow a trace).
//
// Lists return {"items": [...], "next_cursor": "..."}; pass next_cursor as
// ?cursor= to get the next page. A null next_cursor means the last page.
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

type Handler struct {
	logger   *slog.Logger
	store    *store.Store
	projects []config.Project
}

func New(logger *slog.Logger, database *store.Store, projects []config.Project) *Handler {
	return &Handler{logger: logger, store: database, projects: projects}
}

// Register adds the API routes to router, each wrapped with protect
// (IP allowlist and authentication).
//
// Routes are registered one by one instead of as a "/api/v1/" subtree: the
// subtree would overlap the ingest route /api/{projectID}/envelope/, and
// ServeMux rejects overlapping patterns.
func (handler *Handler) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	routes := map[string]http.HandlerFunc{
		"GET /api/v1/projects":                              handler.listProjects,
		"GET /api/v1/issues":                                handler.listIssues,
		"GET /api/v1/issues/{issueID}":                      handler.getIssue,
		"PATCH /api/v1/issues/{issueID}":                    handler.updateIssue,
		"DELETE /api/v1/issues/{issueID}":                   handler.deleteIssue,
		"GET /api/v1/issues/{issueID}/events":               handler.listIssueEvents,
		"GET /api/v1/issues/{issueID}/events/latest":        handler.latestEvent,
		"GET /api/v1/projects/{projectID}/events/{eventID}": handler.getEvent,
		"GET /api/v1/logs":                                  handler.listLogs,
		"GET /api/v1/traces/{traceID}":                      handler.getTrace,
	}
	for pattern, route := range routes {
		router.Handle(pattern, protect(route))
	}
}

// badRequest is an error caused by the client's input, reported as 400.
type badRequest struct{ message string }

func (err badRequest) Error() string { return err.message }

func invalid(format string, args ...any) error {
	return badRequest{fmt.Sprintf(format, args...)}
}

// fail writes the error with the matching status. Unexpected errors are logged
// and reported without details.
func (handler *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var userError badRequest
	switch {
	case errors.As(err, &userError):
		writeError(w, http.StatusBadRequest, userError.message)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		handler.logger.Error("api request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// Cursors are opaque to clients: "o<offset>" for issues (sorted by changing
// fields) or "i<id>" for events and logs (newest first by id).
func encodeCursor(kind byte, value int64) *string {
	cursor := base64.RawURLEncoding.EncodeToString([]byte(string(kind) + strconv.FormatInt(value, 10)))
	return &cursor
}

func decodeCursor(raw string, kind byte) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) < 2 || decoded[0] != kind {
		return 0, invalid("invalid cursor")
	}
	value, err := strconv.ParseInt(string(decoded[1:]), 10, 64)
	if err != nil || value < 0 {
		return 0, invalid("invalid cursor")
	}
	return value, nil
}

func parseLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxLimit {
		return 0, invalid("limit must be between 1 and %d", maxLimit)
	}
	return limit, nil
}

// projectFromQuery resolves ?project= given as an ID ("1") or a name ("backend").
// Returns 0 when the parameter is absent.
func (handler *Handler) projectFromQuery(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("project"))
	if raw == "" {
		return 0, nil
	}
	for _, project := range handler.projects {
		if strconv.Itoa(project.ID) == raw || project.Name == raw {
			return project.ID, nil
		}
	}
	return 0, invalid("unknown project %q", raw)
}

func (handler *Handler) projectName(projectID int) string {
	for _, project := range handler.projects {
		if project.ID == projectID {
			return project.Name
		}
	}
	// Data of a project removed from PROJECTS stays readable.
	return ""
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		return 0, store.ErrNotFound
	}
	return id, nil
}

func parseTimeParam(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, invalid("%s must be an RFC 3339 time, e.g. 2026-11-11T10:00:00Z", name)
	}
	return parsed, nil
}
