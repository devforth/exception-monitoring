package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Retention: deleting old data in small batches, so a large cleanup never
// holds the write lock long enough to delay ingest.
//
// Until RETENTION_ISSUES, an issue is never deleted and keeps its latest
// event: the list of errors and their counts stay complete, and each issue
// can still be opened with its last stack trace. Only older events and logs go.

// notLatestEvent excludes the latest event of each issue. Ordered by the
// events_by_issue_time index only, so it is one index lookup per row.
const notLatestEvent = `events.id != (
	SELECT latest.id FROM events AS latest WHERE latest.issue_id = events.issue_id
	ORDER BY latest.received_at DESC LIMIT 1)`

const retentionBatch = 1000

// deleteInBatches runs a DELETE that takes LIMIT as its last parameter until
// it deletes nothing, and returns the total.
func (store *Store) deleteInBatches(ctx context.Context, query string, args ...any) (int64, error) {
	var total int64

	for {
		result, err := store.DB.ExecContext(ctx, query, append(args, retentionBatch)...)

		if err != nil {
			return total, err
		}

		deleted, _ := result.RowsAffected()
		total += deleted

		if deleted < retentionBatch || ctx.Err() != nil {
			return total, ctx.Err()
		}

		if err := Pause(ctx); err != nil {
			return total, err
		}
	}
}

// workSlice is how long a loop of small writes runs before a WritePause.
const workSlice = 200 * time.Millisecond

// pacer rests after every workSlice of work, for loops of many small writes
// (one per issue or message), where a rest after each would take hours.
type pacer struct{ since time.Time }

func (pacer *pacer) rest(ctx context.Context) error {
	if pacer.since.IsZero() {
		pacer.since = time.Now()
	}

	if time.Since(pacer.since) < workSlice {
		return ctx.Err()
	}

	err := Pause(ctx)
	pacer.since = time.Now()

	return err
}

// condition is the SQL condition on project_id with its arguments.
func scopeCondition(scope ProjectScope) (string, []any) {
	condition, args := inCondition("project_id", scope.IDs)

	switch {
	case condition == "" && scope.Others:
		return "1 = 1", nil
	case condition == "":
		return "1 = 0", nil
	case scope.Others:
		return "NOT " + condition, args
	default:
		return condition, args
	}
}

// DeleteEventsBefore deletes events received before cutoff, except the
// latest event of each issue.
func (store *Store) DeleteEventsBefore(ctx context.Context, scope ProjectScope, cutoff time.Time) (int64, error) {
	condition, args := scopeCondition(scope)

	return store.deleteInBatches(ctx,
		`DELETE FROM events WHERE id IN (
			SELECT id FROM events WHERE `+condition+` AND received_at < ? AND `+notLatestEvent+` ORDER BY received_at LIMIT ?)`,
		append(args, formatTime(cutoff))...)
}

// TrimLogMessages keeps the newest keep logs of each repeated message. New
// logs are trimmed as they arrive (SaveLogs); this applies a lowered limit to
// what is already stored.
func (store *Store) TrimLogMessages(ctx context.Context, scope ProjectScope, keep int) (int64, error) {
	condition, args := scopeCondition(scope)

	// The messages over the limit (a scan of the fingerprint index), then an
	// indexed delete for each.
	rows, err := store.DB.QueryContext(ctx, `
		SELECT project_id, fingerprint FROM logs WHERE fingerprint != '' AND `+condition+`
		GROUP BY project_id, fingerprint HAVING COUNT(*) > ?`, append(args, keep)...)

	if err != nil {
		return 0, err
	}

	var messages []logMessage

	for rows.Next() {
		var message logMessage

		if err := rows.Scan(&message.projectID, &message.fingerprint); err != nil {
			_ = rows.Close()

			return 0, err
		}

		messages = append(messages, message)
	}

	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}

	var (
		total int64
		pace  pacer
	)

	for _, message := range messages {
		deleted, err := trimMessage(ctx, store.DB, message, keep)
		total += deleted

		if err != nil {
			return total, err
		}

		if err := pace.rest(ctx); err != nil {
			return total, err
		}
	}

	return total, nil
}

// DeleteIssuesNotSeenSince deletes issues whose last event came before
// cutoff, with all their events. The events go first, in batches: through
// ON DELETE CASCADE they would all go in one statement, holding the write
// lock for as long as that takes.
func (store *Store) DeleteIssuesNotSeenSince(ctx context.Context, scope ProjectScope, cutoff time.Time) (int64, error) {
	condition, args := scopeCondition(scope)
	args = append(args, formatTime(cutoff))
	expired := `SELECT id FROM issues WHERE ` + condition + ` AND last_seen < ?`

	if _, err := store.deleteInBatches(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM events WHERE issue_id IN (`+expired+`) LIMIT ?)`, args...); err != nil {
		return 0, err
	}

	return store.deleteInBatches(ctx, `DELETE FROM issues WHERE id IN (`+expired+` LIMIT ?)`, args...)
}

// DeleteLogsBefore deletes logs with a timestamp before cutoff.
func (store *Store) DeleteLogsBefore(ctx context.Context, scope ProjectScope, cutoff time.Time) (int64, error) {
	condition, args := scopeCondition(scope)

	return store.deleteInBatches(ctx,
		`DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE `+condition+` AND timestamp < ? ORDER BY timestamp LIMIT ?)`,
		append(args, formatTime(cutoff))...)
}

// TrimIssueEvents keeps the newest keep events of every issue. The issue's
// event_count is left as is: it counts every event ever received, which also
// picks the issues that may have more than keep events stored; each is then
// trimmed through the events_by_issue_time index.
func (store *Store) TrimIssueEvents(ctx context.Context, scope ProjectScope, keep int) (int64, error) {
	condition, args := scopeCondition(scope)
	issueIDs, err := store.selectInt64s(ctx, `SELECT id FROM issues WHERE `+condition+` AND event_count > ?`, append(args, keep)...)

	if err != nil {
		return 0, err
	}

	var (
		total int64
		pace  pacer
	)

	for _, issueID := range issueIDs {
		result, err := store.DB.ExecContext(ctx, `
			DELETE FROM events WHERE id IN (
				SELECT id FROM events WHERE issue_id = ? ORDER BY received_at DESC LIMIT -1 OFFSET ?
			)`, issueID, keep)

		if err != nil {
			return total, err
		}

		deleted, _ := result.RowsAffected()
		total += deleted

		if err := pace.rest(ctx); err != nil {
			return total, err
		}
	}

	return total, nil
}

// DeleteOldest deletes about limit of the oldest records, events (but not the
// latest of an issue) and logs together in time order. Used to bring the database under its size limit,
// so the batch is small: it should not delete much more than needed.
// Returns 0 when there is nothing left to delete.
func (store *Store) DeleteOldest(ctx context.Context, limit int) (int64, error) {
	// The limit-th oldest time over both tables is the cutoff; each query
	// reads at most limit rows of its time index.
	var times []string

	for _, query := range []string{
		`SELECT received_at FROM events WHERE ` + notLatestEvent + ` ORDER BY received_at LIMIT ?`,
		`SELECT timestamp FROM logs ORDER BY timestamp LIMIT ?`,
	} {
		found, err := store.selectStrings(ctx, query, limit)

		if err != nil {
			return 0, err
		}

		times = append(times, found...)
	}

	if len(times) == 0 {
		return 0, nil
	}

	slices.Sort(times)
	cutoff := times[min(limit, len(times))-1]

	var total int64

	for _, query := range []string{
		`DELETE FROM events WHERE id IN (SELECT id FROM events WHERE received_at <= ? AND ` + notLatestEvent + ` ORDER BY received_at LIMIT ?)`,
		`DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp <= ? ORDER BY timestamp LIMIT ?)`,
	} {
		result, err := store.DB.ExecContext(ctx, query, cutoff, limit)

		if err != nil {
			return total, err
		}

		deleted, _ := result.RowsAffected()
		total += deleted
	}

	return total, nil
}

func (store *Store) selectInt64s(ctx context.Context, query string, args ...any) ([]int64, error) {
	return selectColumn[int64](ctx, store.DB, query, args...)
}

func (store *Store) selectStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	return selectColumn[string](ctx, store.DB, query, args...)
}

// selectColumn reads the one column of every row.
func selectColumn[T any](ctx context.Context, db *sql.DB, query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var values []T

	for rows.Next() {
		var value T

		if err := rows.Scan(&value); err != nil {
			return nil, err
		}

		values = append(values, value)
	}

	return values, rows.Err()
}

// CountRecords returns how many events and logs are stored.
func (store *Store) CountRecords(ctx context.Context) (int64, error) {
	var count int64

	err := store.DB.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM events) + (SELECT COUNT(*) FROM logs)`).Scan(&count)

	return count, err
}

// DatabaseSize returns the bytes used by data (the file size minus free pages).
func (store *Store) DatabaseSize(ctx context.Context) (int64, error) {
	var pageCount, freePages, pageSize int64

	for query, target := range map[string]*int64{
		`PRAGMA page_count`:     &pageCount,
		`PRAGMA freelist_count`: &freePages,
		`PRAGMA page_size`:      &pageSize,
	} {
		if err := store.DB.QueryRowContext(ctx, query).Scan(target); err != nil {
			return 0, fmt.Errorf("%s: %w", query, err)
		}
	}

	return (pageCount - freePages) * pageSize, nil
}

// vacuumStep is how many free pages one step gives back (2 MB of 4 KB
// pages): each step is a write transaction, so ingest gets the lock between.
// Moving pages in a large, fragmented file is slow: 2000 took up to 5 s.
const vacuumStep = 500

// ReleaseFreeSpace returns the pages freed by deletes to the file system.
func (store *Store) ReleaseFreeSpace(ctx context.Context) error {
	for ctx.Err() == nil {
		var free int64

		if err := store.DB.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil || free == 0 {
			return err
		}

		// The pragma returns a row per page; draining the rows runs the step.
		rows, err := store.DB.QueryContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, vacuumStep))

		if err != nil {
			return err
		}

		for rows.Next() { // the rows only drive the pragma
		}

		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}

		if err := Pause(ctx); err != nil {
			return err
		}
	}

	return ctx.Err()
}
