package ingest

import (
	"context"
	"log/slog"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/sentry"
)

// LogSink writes received data to the server log instead of storing it.
// Temporary until the store has tables for events and logs.
type LogSink struct {
	Logger *slog.Logger
}

func (sink LogSink) StoreEvent(_ context.Context, project config.Project, event *sentry.Event) error {
	sink.Logger.Info("event received",
		"project", project.Name,
		"event_id", event.EventID,
		"level", event.Level,
		"title", event.Title(),
		"environment", event.Environment,
		"platform", event.Platform,
	)
	return nil
}

func (sink LogSink) StoreLogs(_ context.Context, project config.Project, logs []sentry.Log) error {
	sink.Logger.Info("logs received", "project", project.Name, "count", len(logs))
	for _, record := range logs {
		sink.Logger.Debug("log", "project", project.Name, "level", record.Level, "body", record.Body, "trace_id", record.TraceID)
	}
	return nil
}
