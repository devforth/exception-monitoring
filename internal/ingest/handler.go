// Package ingest receives envelopes from Sentry SDKs at
// POST /api/{projectID}/envelope/ and stores their events and logs.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/devforth/exception-monitoring/internal/ingest/scrub"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/server/httpjson"
)

// Sink receives parsed data. Its errors are returned to the SDK as 500.
type Sink interface {
	StoreEvent(ctx context.Context, project config.Project, event *sentry.Event) error
	StoreLogs(ctx context.Context, project config.Project, logs []sentry.Log) error
}

type Handler struct {
	logger   *slog.Logger
	projects config.ProjectList // read per request: projects can change while running
	sink     Sink
	cors     cors
	limiter  *rateLimiter // nil when rate limiting is disabled
	now      func() time.Time
	// pipeline, when set, takes envelopes into its inbox and stores them
	// later; without it they are processed in the request.
	pipeline *Pipeline
}

func New(logger *slog.Logger, settings *config.Config, projects config.ProjectList, sink Sink) *Handler {
	handler := &Handler{
		logger:   logger,
		projects: projects,
		sink:     sink,
		cors:     newCORS(settings.AllowedOrigins),
		now:      time.Now,
	}

	if settings.RateLimitPerMinute > 0 {
		handler.limiter = newRateLimiter(settings.RateLimitPerMinute)
	}

	return handler
}

// requestError is a rejection that is reported to the SDK with an HTTP status.
type requestError struct {
	status     int
	message    string
	retryAfter string // seconds, only for 429
}

func (err *requestError) Error() string { return err.message }

func reject(status int, format string, args ...any) error {
	return &requestError{status: status, message: fmt.Sprintf(format, args...)}
}

func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	originAllowed := handler.cors.apply(w, r)

	if r.Method == http.MethodOptions {
		if !originAllowed {
			writeError(w, http.StatusForbidden, "origin not allowed")
			return
		}

		w.WriteHeader(http.StatusOK)

		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")

		return
	}

	if !originAllowed {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}

	response, err := handler.ingest(w, r)

	if err != nil {
		var rejection *requestError

		if errors.As(err, &rejection) {
			handler.logger.Debug("envelope rejected", "path", r.URL.Path, "status", rejection.status, "reason", rejection.message)

			if rejection.retryAfter != "" {
				writeRateLimited(w, rejection.retryAfter)
				return
			}

			writeError(w, rejection.status, rejection.message)

			return
		}

		handler.logger.Error("ingest failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")

		return
	}

	httpjson.Write(w, http.StatusOK, response)
}

func (handler *Handler) ingest(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	projectID, err := strconv.Atoi(r.PathValue("projectID"))

	if err != nil {
		return nil, reject(http.StatusNotFound, "invalid project id")
	}

	// Usually the key is in the request. Behind a tunnel it is only in the
	// envelope header, so the body has to be parsed before authenticating.
	var (
		envelope *sentry.Envelope
		body     []byte
	)

	key := publicKeyFromRequest(r)

	if key == "" {
		if envelope, body, err = readEnvelope(w, r); err != nil {
			return nil, err
		}

		key = publicKeyFromDSN(envelope.Header.DSN, projectID)
	}

	if key == "" {
		return nil, reject(http.StatusUnauthorized, "missing sentry_key")
	}

	project, ok := handler.projects.List().ByID(projectID)

	if !ok || key != project.PublicKey {
		return nil, reject(http.StatusForbidden, "invalid project or key")
	}

	if handler.limiter != nil {
		if retryAfter, allowed := handler.limiter.allow(projectID, handler.now()); !allowed {
			return nil, &requestError{status: http.StatusTooManyRequests, message: "rate limit exceeded", retryAfter: retryAfter}
		}
	}

	if envelope == nil {
		if envelope, body, err = readEnvelope(w, r); err != nil {
			return nil, err
		}
	}

	if handler.pipeline != nil {
		return handler.pipeline.accept(project, envelope, body)
	}

	return handler.process(r.Context(), project, envelope)
}

// process parses every item before storing anything, so a malformed item
// rejects the whole envelope instead of storing half of it.
func (handler *Handler) process(ctx context.Context, project config.Project, envelope *sentry.Envelope) (map[string]string, error) {
	events, logs, err := handler.parseItems(project, envelope, handler.now())

	if err != nil {
		return nil, err
	}

	for _, event := range events {
		if err := handler.sink.StoreEvent(ctx, project, event); err != nil {
			return nil, fmt.Errorf("store event: %w", err)
		}
	}

	if len(logs) > 0 {
		if err := handler.sink.StoreLogs(ctx, project, logs); err != nil {
			return nil, fmt.Errorf("store logs: %w", err)
		}
	}

	response := map[string]string{}

	if len(events) > 0 {
		response["id"] = events[0].EventID
	}

	return response, nil
}

// parseItems parses the events and logs of an envelope; other item types are
// dropped. Any malformed item fails the whole envelope.
func (handler *Handler) parseItems(project config.Project, envelope *sentry.Envelope, received time.Time) ([]*sentry.Event, []sentry.Log, error) {
	var (
		events []*sentry.Event
		logs   []sentry.Log
	)

	dropped := map[string]int{}
	drift := clockDrift(envelope.Header.SentAt, received)

	for index, item := range envelope.Items {
		switch item.Header.Type {
		case sentry.ItemEvent:
			// Credentials (cookies, Authorization, tokens) are removed before
			// anything is parsed or stored (package scrub), and so is what
			// does not help a fix, such as library source (package compact).
			event, err := sentry.ParseEvent(prepareEvent(item.Payload), envelope.Header.EventID, received)

			if err != nil {
				return nil, nil, reject(http.StatusBadRequest, "item %d: %v", index+1, err)
			}

			if !event.Timestamp.Equal(received) { // received: the event had no time of its own
				event.Timestamp.Time = event.Timestamp.Add(drift)
			}

			events = append(events, event)
		case sentry.ItemLog:
			batch, err := sentry.ParseLogs(scrub.JSON(item.Payload), received)

			if err != nil {
				return nil, nil, reject(http.StatusBadRequest, "item %d: %v", index+1, err)
			}

			for index := range batch {
				if !batch[index].Timestamp.Equal(received) {
					batch[index].Timestamp.Time = batch[index].Timestamp.Add(drift)
				}
			}

			logs = append(logs, batch...)
		default:
			dropped[item.Header.Type]++
		}
	}

	if len(dropped) > 0 {
		handler.logger.Debug("dropped unsupported items", "project", project.Name, "types", dropped)
	}

	return events, logs, nil
}

// readEnvelope reads the body (decompressed) and splits it into items.
func readEnvelope(w http.ResponseWriter, r *http.Request) (*sentry.Envelope, []byte, error) {
	body, err := readBody(w, r)

	if err != nil {
		return nil, nil, err
	}

	envelope, err := sentry.ParseEnvelope(body)

	if err != nil {
		return nil, nil, reject(http.StatusBadRequest, "%v", err)
	}

	return envelope, body, nil
}

// Errors use Sentry's {"detail": "..."} shape; SDKs log it on failure.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("X-Sentry-Error", message)
	httpjson.Write(w, status, map[string]string{"detail": message})
}

// writeRateLimited tells the SDK to pause sending. X-Sentry-Rate-Limits is
// "retry_after:categories:scope"; empty categories mean all data types.
func writeRateLimited(w http.ResponseWriter, retryAfter string) {
	w.Header().Set("Retry-After", retryAfter)
	w.Header().Set("X-Sentry-Rate-Limits", retryAfter+"::project")
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
}

// clockDrift is how far the SDK's clock was behind the server's when it sent
// the envelope (sent_at), so the times of its events and logs can be moved by
// it; nothing under a minute, which network delays explain.
func clockDrift(sentAt string, received time.Time) time.Duration {
	sent, err := time.Parse(time.RFC3339Nano, sentAt)

	if err != nil {
		return 0
	}

	if drift := received.Sub(sent); drift.Abs() >= time.Minute {
		return drift
	}

	return 0
}
