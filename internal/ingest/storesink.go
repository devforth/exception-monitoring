package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/grouping"
	"github.com/devforth/exception-monitoring/internal/notify"
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
	// Notifier is told about new and regressed issues. Nil sends nothing.
	Notifier Notifier
	// PublicURL is the UI address for links in notifications; empty means no link.
	PublicURL string
}

// Notifier queues alerts without blocking ingest (see package notify).
type Notifier interface {
	Notify(notification notify.Notification)
}

func (sink StoreSink) now() time.Time {
	if sink.Now != nil {
		return sink.Now()
	}

	return time.Now()
}

func (sink StoreSink) StoreEvent(ctx context.Context, project config.Project, event *sentry.Event) error {
	trace := event.Trace()

	receivedAt := sink.now()

	result, err := sink.Store.SaveEvent(ctx, store.NewEvent{
		ProjectID:    project.ID,
		EventID:      event.EventID,
		Fingerprint:  grouping.Fingerprint(event),
		Title:        event.Title(),
		Culprit:      grouping.Culprit(event),
		Level:        event.Level,
		Environment:  event.Environment,
		Release:      event.Release,
		TraceID:      trace.TraceID,
		SpanID:       trace.SpanID,
		ParentSpanID: trace.ParentSpanID,
		Timestamp:    event.Timestamp.Time,
		ReceivedAt:   receivedAt,
		Payload:      event.Raw,
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
		sink.notify(notify.NewIssue, project, result.IssueID, event, receivedAt)
	case result.Regression:
		sink.Logger.Info("issue regressed", attributes...)
		sink.notify(notify.Regression, project, result.IssueID, event, receivedAt)
	default:
		sink.Logger.Debug("event stored", attributes...)
	}

	return nil
}

func (sink StoreSink) notify(kind notify.Kind, project config.Project, issueID int64, event *sentry.Event, receivedAt time.Time) {
	if sink.Notifier == nil {
		return
	}

	issueURL := ""

	if sink.PublicURL != "" {
		issueURL = fmt.Sprintf("%s/issues/%d", sink.PublicURL, issueID)
	}

	sink.Notifier.Notify(notify.Notification{
		Kind:        kind,
		ProjectID:   project.ID,
		Project:     project.Name,
		IssueID:     issueID,
		IssueURL:    issueURL,
		Title:       event.Title(),
		Culprit:     grouping.Culprit(event),
		Level:       event.Level,
		Environment: event.Environment,
		Release:     event.Release,
		Time:        receivedAt,
		Event:       event,
	})
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
			SpanID:         record.SpanID,
			Attributes:     attributes,
		})
	}

	if err := sink.Store.SaveLogs(ctx, records); err != nil {
		return err
	}

	sink.Logger.Debug("logs stored", "project", project.Name, "count", len(records))

	return nil
}
