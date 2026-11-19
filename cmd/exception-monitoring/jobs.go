package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/background"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/database"
)

// lanes is every periodic job of the server (schedule: config/schedule.go).
// Besides them, the ingest pipeline, the notifier's dispatcher and workers and
// the HTTP server run as long as the server does.
func lanes(db *database.Store, notifier *notify.Notifier, maxBytes int64, logger *slog.Logger) []background.Lane {
	return []background.Lane{
		{Name: "database", Jobs: []background.Job{
			{Name: "inbox compaction", AtStart: true, First: config.InboxCompactionAt, Every: config.CleanupInterval, Timeout: config.InboxCompactionTimeout,
				Note: "when ingest is quiet", Run: db.CompactInbox},
			{Name: "size check", AtStart: true, First: config.CleanupInterval, Every: config.CleanupInterval, Timeout: config.SizeCheckTimeout,
				Wake: db.CleanupWake(), Note: fmt.Sprintf("after %s written", config.FormatSize(config.CleanupWritten)),
				Run: func(ctx context.Context) error { return db.CheckSize(ctx, maxBytes, logger) }},
		}},
		{Name: "notifications", Jobs: []background.Job{
			{Name: "outbox cleanup", First: config.OutboxCleanupDelay, Every: config.OutboxCleanupInterval, Timeout: config.OutboxCleanupTimeout,
				Run: notifier.CleanOutbox},
		}},
	}
}
