// Package retention keeps the database within the limits of config.Retention:
// events and logs older than their retention period are deleted, every issue
// keeps only its newest events, and above MAX_DATABASE_SIZE the oldest data
// goes first. Each issue keeps its latest event, so every error stays listed
// with its count and last stack trace, until it has not happened for
// RETENTION_ISSUES: then the issue is deleted with its events.
//
// Every limit but the size can differ per project (set on the Projects page);
// projects without their own values use config.Retention.
//
// It runs at startup and then every Interval. Deletes run in small batches
// (see store), so ingest keeps working during a large cleanup.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// DefaultInterval between cleanups. The size limit is checked as often, so
// a burst of errors cannot fill the disk for long.
const DefaultInterval = 10 * time.Minute

// sizeTarget: above the size limit, data is deleted down to this share of
// it, so the next few events do not trigger another cleanup right away.
const sizeTarget = 0.9

// maxOversizeBatch caps how many of the oldest records go per step when the
// database is over its size limit; the size is measured after each step.
const maxOversizeBatch = 1000

// Store is what retention needs from the database.
type Store interface {
	DeleteEventsBefore(ctx context.Context, scope store.ProjectScope, cutoff time.Time) (int64, error)
	DeleteLogsBefore(ctx context.Context, scope store.ProjectScope, cutoff time.Time) (int64, error)
	TrimIssueEvents(ctx context.Context, scope store.ProjectScope, keep int) (int64, error)
	TrimLogMessages(ctx context.Context, scope store.ProjectScope, keep int) (int64, error)
	DeleteIssuesNotSeenSince(ctx context.Context, scope store.ProjectScope, cutoff time.Time) (int64, error)
	DeleteOldest(ctx context.Context, limit int) (int64, error)
	CountRecords(ctx context.Context) (int64, error)
	DatabaseSize(ctx context.Context) (int64, error)
	ReleaseFreeSpace(ctx context.Context) error
	RefreshLogSearch(ctx context.Context) (bool, error)
}

var _ Store = (store.Store)(nil)

type Cleaner struct {
	Store    Store
	Settings config.Retention // global; projects may override parts of it
	Projects config.ProjectList
	Logger   *slog.Logger
	Interval time.Duration    // zero means DefaultInterval
	Now      func() time.Time // nil means time.Now
}

// Result counts what one cleanup deleted.
type Result struct {
	ExpiredEvents  int64 // older than RETENTION_EVENTS
	ExpiredLogs    int64 // older than RETENTION_LOGS
	TrimmedLogs    int64 // beyond RETENTION_LOGS_PER_MESSAGE
	TrimmedEvents  int64 // beyond RETENTION_EVENTS_PER_ISSUE
	OversizeEvents int64 // events and logs deleted for MAX_DATABASE_SIZE
	ExpiredIssues  int64 // not seen for RETENTION_ISSUES, deleted with their events
	SizeBytes      int64 // used by data after the cleanup
}

func (result Result) deletedAnything() bool {
	return result.ExpiredEvents+result.ExpiredLogs+result.TrimmedEvents+result.TrimmedLogs+result.OversizeEvents+result.ExpiredIssues > 0
}

// Run cleans up until ctx is cancelled.
func (cleaner *Cleaner) Run(ctx context.Context) {
	interval := cleaner.Interval

	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)

	defer ticker.Stop()

	for {
		if _, err := cleaner.Clean(ctx); err != nil && ctx.Err() == nil {
			cleaner.Logger.Error("retention cleanup failed", "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Clean applies every limit once.
func (cleaner *Cleaner) Clean(ctx context.Context) (Result, error) {
	var (
		result Result
		err    error
	)

	settings := cleaner.Settings
	now := cleaner.now()

	for _, group := range cleaner.groups() {
		if err := cleaner.applyLimits(ctx, group.scope, group.settings, now, &result); err != nil {
			return result, err
		}
	}

	if result.SizeBytes, err = cleaner.Store.DatabaseSize(ctx); err != nil {
		return result, err
	}

	if limit := settings.MaxDatabaseBytes; limit > 0 && result.SizeBytes > limit {
		target := int64(float64(limit) * sizeTarget)
		records, err := cleaner.Store.CountRecords(ctx)

		if err != nil {
			return result, err
		}

		// The average record size tells how many records to delete per step.
		averageSize := result.SizeBytes / max(records, 1)

		for result.SizeBytes > target {
			batch := min((result.SizeBytes-target)/max(averageSize, 1)+1, maxOversizeBatch)
			deleted, err := cleaner.Store.DeleteOldest(ctx, int(batch))

			if err != nil {
				return result, err
			}

			if deleted == 0 {
				// Only the latest event of each issue is left: it stays.
				cleaner.Logger.Warn("database is over MAX_DATABASE_SIZE with only the latest event of each issue left",
					"size", config.FormatSize(result.SizeBytes), "limit", config.FormatSize(limit))

				break
			}

			result.OversizeEvents += deleted

			// Deleted logs keep their place in the search index until it is
			// filled anew; past that point emptying it frees the most.
			if err := cleaner.refreshLogSearch(ctx); err != nil {
				return result, err
			}

			if err := store.Pause(ctx); err != nil {
				return result, err
			}

			if result.SizeBytes, err = cleaner.Store.DatabaseSize(ctx); err != nil {
				return result, err
			}
		}
	}

	if !result.deletedAnything() {
		return result, nil
	}

	if err := cleaner.refreshLogSearch(ctx); err != nil {
		return result, err
	}

	if err := cleaner.Store.ReleaseFreeSpace(ctx); err != nil {
		return result, err
	}

	if result.SizeBytes, err = cleaner.Store.DatabaseSize(ctx); err != nil {
		return result, err
	}

	cleaner.Logger.Info("retention cleanup",
		"expired_events", result.ExpiredEvents, "expired_logs", result.ExpiredLogs,
		"trimmed_events", result.TrimmedEvents, "trimmed_logs", result.TrimmedLogs, "deleted_for_size", result.OversizeEvents, "expired_issues", result.ExpiredIssues,
		"database_size", config.FormatSize(result.SizeBytes))

	return result, nil
}

type projectGroup struct {
	scope    store.ProjectScope
	settings config.Retention
}

// groups splits the data by the retention that applies to it: one group per
// distinct project setting, and one with the global settings for every other
// project (including data of deleted projects).
func (cleaner *Cleaner) groups() []projectGroup {
	byRetention := map[config.Retention][]int{}

	var overridden []int

	if cleaner.Projects != nil {
		for _, project := range cleaner.Projects.List() {
			if own := cleaner.Settings.For(project.Retention); own != cleaner.Settings {
				byRetention[own] = append(byRetention[own], project.ID)
				overridden = append(overridden, project.ID)
			}
		}
	}

	groups := []projectGroup{{scope: store.ProjectScope{IDs: overridden, Others: true}, settings: cleaner.Settings}}

	for settings, ids := range byRetention {
		groups = append(groups, projectGroup{scope: store.ProjectScope{IDs: ids}, settings: settings})
	}

	return groups
}

// applyLimits applies the time and count limits to one group of projects.
func (cleaner *Cleaner) applyLimits(ctx context.Context, scope store.ProjectScope, settings config.Retention, now time.Time, result *Result) error {
	steps := []struct {
		enabled bool
		run     func() (int64, error)
		count   *int64
	}{
		{settings.Issues > 0, func() (int64, error) {
			return cleaner.Store.DeleteIssuesNotSeenSince(ctx, scope, now.Add(-settings.Issues))
		}, &result.ExpiredIssues},
		{settings.Events > 0, func() (int64, error) {
			return cleaner.Store.DeleteEventsBefore(ctx, scope, now.Add(-settings.Events))
		}, &result.ExpiredEvents},
		{settings.Logs > 0, func() (int64, error) { return cleaner.Store.DeleteLogsBefore(ctx, scope, now.Add(-settings.Logs)) }, &result.ExpiredLogs},
		{settings.EventsPerIssue > 0, func() (int64, error) {
			return cleaner.Store.TrimIssueEvents(ctx, scope, settings.EventsPerIssue)
		}, &result.TrimmedEvents},
		{settings.LogsPerMessage > 0, func() (int64, error) {
			return cleaner.Store.TrimLogMessages(ctx, scope, settings.LogsPerMessage)
		}, &result.TrimmedLogs},
	}

	for _, step := range steps {
		if !step.enabled {
			continue
		}

		deleted, err := step.run()
		*step.count += deleted

		if err != nil {
			return err
		}
	}

	return nil
}

func (cleaner *Cleaner) now() time.Time {
	if cleaner.Now != nil {
		return cleaner.Now()
	}

	return time.Now()
}

// refreshLogSearch empties the search index when it holds mostly deleted
// logs; the background indexing fills it again.
func (cleaner *Cleaner) refreshLogSearch(ctx context.Context) error {
	refreshed, err := cleaner.Store.RefreshLogSearch(ctx)

	if refreshed {
		cleaner.Logger.Info("log search index holds mostly deleted logs, filling it anew in the background")
	}

	return err
}
