package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/grouping"
	"github.com/devforth/exception-monitoring/internal/sentry"
	"github.com/devforth/exception-monitoring/internal/store"
)

// StoreSink groups events into issues and saves events and logs to the database.
// It is the bridge between the Sentry format and the store, which knows
// nothing about Sentry.
type StoreSink struct {
	Store  *store.Store
	Logger *slog.Logger
	Now    func() time.Time // nil means time.Now
}

func (sink StoreSink) now() time.Time {
	if sink.Now != nil {
		return sink.Now()
	}

	return time.Now()
}

func (sink StoreSink) StoreEvent(ctx context.Context, project config.Project, event *sentry.Event) error {
	result, err := sink.Store.SaveEvent(ctx, store.NewEvent{
		ProjectID:   project.ID,
		EventID:     event.EventID,
		Fingerprint: grouping.Fingerprint(event),
		Title:       event.Title(),
		Culprit:     grouping.Culprit(event),
		Level:       event.Level,
		Environment: event.Environment,
		Release:     event.Release,
		TraceID:     event.TraceID(),
		Timestamp:   event.Timestamp.Time,
		ReceivedAt:  sink.now(),
		Payload:     event.Raw,
	})

	if err != nil {
		return err
	}

	attributes := []any{"project", project.Name, "issue_id", result.IssueID, "event_id", event.EventID, "title", event.Title()}

	switch {
	case result.Duplicate:
		sink.Logger.Debug("duplicate event ignored", attributes...)
	case result.NewIssue:
		sink.Logger.Info("new issue", attributes...)
	case result.Regression:
		sink.Logger.Info("issue regressed", attributes...)
	default:
		sink.Logger.Debug("event stored", attributes...)
	}

	return nil
}

func (sink StoreSink) StoreLogs(ctx context.Context, project config.Project, logs []sentry.Log) error {
	records := make([]store.NewLog, 0, len(logs))

	for _, record := range logs {
		attributes, err := json.Marshal(record.Attributes)

		if err != nil {
			return fmt.Errorf("log attributes: %w", err)
		}

		records = append(records, store.NewLog{
			ProjectID:      project.ID,
			Timestamp:      record.Timestamp.Time,
			Level:          record.Level,
			SeverityNumber: record.SeverityNumber,
			Body:           record.Body,
			TraceID:        record.TraceID,
			Attributes:     attributes,
		})
	}

	if err := sink.Store.SaveLogs(ctx, records); err != nil {
		return err
	}

	sink.Logger.Debug("logs stored", "project", project.Name, "count", len(records))

	return nil
}
