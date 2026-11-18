package database

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/logging"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// The cleanup by size; what it deletes and in which order: docs/configuration.md (Retention).
const (
	cleanupStart      = float64(config.CleanupStartPercent) / 100
	cleanupStop       = float64(config.CleanupStopPercent) / 100
	cleanupAge        = config.CleanupAge
	cleanupRecentAge  = config.CleanupRecentAge
	cleanupKeepEvents = config.OldEventsPerIssue
	cleanupChunk      = 2000
	cleanupRest       = 2 * time.Millisecond
	cleanupEvery      = 50 << 20
	cleanupHistory    = 100 // runs kept
)

// The records of a run are the store's.
type (
	CleanupRun    = model.CleanupRun
	CleanupStep   = model.CleanupStep
	CleanupCounts = model.CleanupCounts
)

const (
	cleanupRunning = model.CleanupRunning
	cleanupDone    = model.CleanupDone
	cleanupFailed  = model.CleanupFailed
)

// RunCleanup runs ReduceAging and Cleanup now, every interval and after every cleanupEvery bytes written.
func (database *Store) RunCleanup(ctx context.Context, maxBytes int64, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)

	defer ticker.Stop()

	for {
		if maxBytes > 0 {
			if reduced, err := database.ReduceAging(ctx); err != nil && ctx.Err() == nil {
				logger.Error("thinning old events failed", "err", err)
			} else if len(reduced) > 0 {
				logger.Debug("old events thinned", "step", stepLine(reduced[0]))
			}
		}

		run, err := database.Cleanup(ctx, maxBytes)

		switch {
		case ctx.Err() != nil:
		case err != nil:
			logger.Error("cleanup failed", "err", err, "reached", fmt.Sprintf("step %d", run.Reached))
		case run.Status == cleanupDone:
			logCleanup(logger, run)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-database.cleanupWake:
		}
	}
}

// logCleanup writes a run as one record: sizes, the last step reached and a line per step that deleted something.
func logCleanup(logger *slog.Logger, run CleanupRun) {
	attributes := []any{logging.Block, "limit", config.FormatSize(run.Limit), "size_before", config.FormatSize(run.SizeBefore),
		"compacted", config.FormatSize(run.Compacted), "size_after", config.FormatSize(run.SizeAfter)}

	if run.Reached > 0 {
		attributes = append(attributes, "reached", fmt.Sprintf("step %d", run.Reached))
	} else {
		attributes = append(attributes, "deleted", "nothing, compacting was enough")
	}

	attributes = append(attributes, "duration", run.Duration.Round(time.Millisecond))

	for _, step := range run.Steps {
		attributes = append(attributes, fmt.Sprintf("step_%d", step.Number), stepLine(step))
	}

	logger.Info("cleanup", attributes...)
}

// stepLine reads "logs below error, freed 1.2 MB in 40ms: project 1: 300 logs; project 2: 12 logs".
func stepLine(step CleanupStep) string {
	var projects []string

	for _, project := range slices.Sorted(maps.Keys(step.Projects)) {
		counts := step.Projects[project]

		var parts []string

		for _, part := range []struct {
			count int
			name  string
		}{{counts.Logs, "logs"}, {counts.Events, "events"}, {counts.Thinned, "events thinned"}, {counts.Issues, "issues"}} {
			if part.count > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", part.count, part.name))
			}
		}

		projects = append(projects, fmt.Sprintf("project %d: %s", project, strings.Join(parts, ", ")))
	}

	return fmt.Sprintf("%s, freed %s in %s: %s", step.Name, config.FormatSize(step.Freed), step.Duration.Round(time.Millisecond),
		strings.Join(projects, "; "))
}

func (database *Store) noteWritten() {
	written := database.db.Written()

	if written-database.checkedAt.Load() < cleanupEvery {
		return
	}

	database.checkedAt.Store(written)

	select {
	case database.cleanupWake <- struct{}{}:
	default:
	}
}

// Cleanup goes on with a run a restart stopped, or starts one when the
// database is at cleanupStart of maxBytes (0: no limit). A run stopped by ctx
// stays running and goes on at the next call.
func (database *Store) Cleanup(ctx context.Context, maxBytes int64) (CleanupRun, error) {
	if maxBytes <= 0 {
		return CleanupRun{}, nil
	}

	latest, err := database.cleanups.Primary().Newest().First(database.db)
	run := latest.Record

	switch {
	case err == nil && run.Status == cleanupRunning:
	case err == nil || errors.Is(err, kv.ErrNotFound):
		run, err = database.startCleanup(maxBytes)

		if err != nil || run.Status != cleanupRunning {
			return run, err
		}
	default:
		return CleanupRun{}, err
	}

	err = database.advance(ctx, &run)

	if ctx.Err() != nil {
		return run, err
	}

	return run, database.finishCleanup(&run, err)
}

// startCleanup compacts first what collects deleted and replaced records as
// the server works (the inbox, the issues rewritten with every event): they
// keep their space until then, and that may be enough. Otherwise it saves a
// run whose goal is cleanupStop of maxBytes.
func (database *Store) startCleanup(maxBytes int64) (CleanupRun, error) {
	run := CleanupRun{StartedAt: database.now().UTC(), Limit: maxBytes}
	size, err := database.db.Size()

	if err != nil || float64(size) < cleanupStart*float64(maxBytes) {
		return run, err
	}

	run.SizeBefore = size

	for _, keyRange := range []func() ([]byte, []byte){database.inbox.KeyRange, database.issues.KeyRange, database.status.KeyRange} {
		if err := database.db.CompactRange(keyRange()); err != nil {
			return run, err
		}
	}

	if run.Compacted, err = database.db.Size(); err != nil {
		return run, err
	}

	if float64(run.Compacted) < cleanupStart*float64(maxBytes) {
		run.Status, run.SizeAfter = cleanupDone, run.Compacted
		run.Duration = database.now().Sub(run.StartedAt)

		return run, database.saveCleanup(&run)
	}

	// The counters count bytes before compression; the ratio converts.
	logical := database.logicalSize()
	ratio := float64(run.Compacted) / float64(max(logical, 1))
	run.Goal = logical - int64((float64(run.Compacted)-cleanupStop*float64(maxBytes))/ratio)
	run.Status, run.Reached = cleanupRunning, 1

	return run, database.saveCleanup(&run)
}

// finishCleanup compacts what the run deleted from and saves it as done or failed.
func (database *Store) finishCleanup(run *CleanupRun, err error) error {
	if err == nil {
		err = database.compactCleaned(run)
	}

	if err == nil {
		run.SizeAfter, err = database.db.Size()
	}

	run.Status, run.Duration = cleanupDone, database.now().Sub(run.StartedAt)

	if err != nil {
		run.Status, run.Error = cleanupFailed, err.Error()
	}

	return errors.Join(err, database.db.Update(true, func(batch *kv.Batch) error { return database.cleanups.Save(batch, *run) }))
}

// saveCleanup stores a new run and keeps the newest cleanupHistory of them.
func (database *Store) saveCleanup(run *CleanupRun) error {
	sequence, err := database.db.Next()

	if err != nil {
		return err
	}

	run.Sequence = sequence

	return database.db.Update(true, func(batch *kv.Batch) error {
		if err := database.cleanups.Insert(batch, *run); err != nil {
			return err
		}

		var stored []kv.Key

		err := database.cleanups.Primary().Keys(database.db, func(key kv.Key) (bool, error) {
			stored = append(stored, key)

			return true, nil
		})

		for _, key := range stored[:max(0, len(stored)+1-cleanupHistory)] {
			if err == nil {
				_, err = database.cleanups.DeleteKey(batch, key)
			}
		}

		return err
	})
}

func (database *Store) CleanupHistory(_ context.Context, limit int) ([]CleanupRun, error) {
	return database.cleanups.Primary().Newest().Limit(limit).All(database.db)
}

// compactCleaned compacts the collections the run deleted from, so the disk space comes back now.
func (database *Store) compactCleaned(run *CleanupRun) error {
	var logs, events, issues bool

	for _, step := range run.Steps {
		for _, counts := range step.Projects {
			logs = logs || counts.Logs > 0
			events = events || counts.Events > 0 || counts.Thinned > 0
			issues = issues || counts.Issues > 0
		}
	}

	var ranges []func() ([]byte, []byte)

	if logs {
		ranges = append(ranges, database.logs.KeyRange)
	}

	if events {
		ranges = append(ranges, database.events.KeyRange)
	}

	if issues {
		ranges = append(ranges, database.issues.KeyRange, database.status.KeyRange)
	}

	for _, keyRange := range ranges {
		if err := database.db.CompactRange(keyRange()); err != nil {
			return err
		}
	}

	return nil
}

func (database *Store) logicalSize() int64 {
	var total int64

	for _, size := range database.db.Counters() {
		total += size
	}

	return total
}

// Logs below error go whenever the cleanup runs; error and fatal ones only while needed.
var (
	minorLogRanks = allRanks[:levelRank("error")]
	majorLogRanks = allRanks[levelRank("error"):]
)

// cleanupStep deletes in batches: each batch reads the data as it is then,
// under the write lock, so what arrives meanwhile is judged by its rule too.
type cleanupStep struct {
	name   string
	always bool // runs in full; the others stop once the goal is reached
	batch  cleanupBatch
}

type cleanupBatch func(writer *batchWriter, cursor *cleanupCursor) (finished bool, err error)

// cleanupCursor is one batch of a step: where it starts and goes on, and how much it may do.
type cleanupCursor struct {
	batch    *kv.Batch
	position []byte
	need     int64 // bytes still to free; only steps that are not always stop there
	always   bool
	records  int
}

// full says the batch has done enough: cleanupChunk records, or freed what is needed.
func (cursor *cleanupCursor) full() bool {
	return cursor.records >= cleanupChunk || (!cursor.always && -cursor.batch.Counted() >= cursor.need)
}

func (cursor *cleanupCursor) number(offset int) uint64 {
	return kv.Decode(cursor.position, offset).U64()
}

// cleanupSteps are the steps in the order of docs/configuration.md; their number is their place.
func (database *Store) cleanupSteps() []cleanupStep {
	now := database.now()
	month, week := now.Add(-cleanupAge), now.Add(-cleanupRecentAge)

	return []cleanupStep{
		{"logs older than a month", true, database.deleteLogs(allRanks, func() uint64 { return database.sequenceFrom(month) })},
		{"events older than a month beyond the newest of their issue", true, database.trimIssues(cleanupKeepEvents, month, func(issue *issueRecord) bool {
			return len(issue.EventDays) > 0 && issue.EventDays[0].Day <= dayOf(month)
		})},
		{"events of resolved and ignored issues but the latest", true, database.trimIssues(1, time.Time{}, func(issue *issueRecord) bool {
			return issue.Status != model.StatusUnresolved
		})},
		{"logs below error", true, database.deleteLogs(minorLogRanks, nil)},
		{"error and fatal logs", false, database.deleteOldestLogs(majorLogRanks)},
		{"events beyond the newest of their issue", false, database.trimIssues(cleanupKeepEvents, time.Time{}, nil)},
		{"events older than a week", false, database.deleteOldestEvents(week)},
		{"events of the last week", false, database.deleteOldestEvents(time.Time{})},
		{"latest events of issues not seen for a week", false, database.deleteUnseen(week, false)},
		{"whole issues", false, database.deleteUnseen(time.Time{}, true)},
	}
}

// agingSteps thin the events older than a month as they age, in the background; they delete nothing.
func (database *Store) agingSteps() []cleanupStep {
	return []cleanupStep{{"details of events older than a month", true, database.thinBefore(database.now().Add(-cleanupAge))}}
}

// ReduceAging runs agingSteps to the end and says what they did; only what
// aged since the last call is left to do, so it is cheap when run often.
func (database *Store) ReduceAging(ctx context.Context) ([]CleanupStep, error) {
	var done []CleanupStep

	for _, step := range database.agingSteps() {
		run := CleanupRun{Reached: 1}

		for finished := false; !finished && ctx.Err() == nil; {
			started := time.Now()

			err := database.update(func(writer *batchWriter) error {
				writer.counted = map[int]*CleanupCounts{}
				cursor := &cleanupCursor{batch: writer.batch, position: run.Position, always: true}

				var err error

				finished, err = step.batch(writer, cursor)
				// The run is in memory only: a restart starts over, and the steps skip what is done.
				writer.batch.AfterWrite(func() {
					run = withBatch(run, step.name, writer.counted, -writer.batch.Counted(), time.Since(started))
					run.Position = cursor.position
				})

				return err
			})

			if err != nil {
				return done, err
			}

			if !finished {
				select {
				case <-ctx.Done():
				case <-time.After(cleanupRest):
				}
			}
		}

		done = append(done, run.Steps...)
	}

	return done, ctx.Err()
}

// advance runs the run's steps batch by batch from where it is; each batch
// saves the run with it, so the progress and the deletes are written together.
func (database *Store) advance(ctx context.Context, run *CleanupRun) error {
	steps := database.cleanupSteps()

	for ctx.Err() == nil {
		if run.Reached > len(steps) {
			run.Reached = len(steps)

			return nil
		}

		step := steps[run.Reached-1]
		need := database.logicalSize() - run.Goal

		if !step.always && need <= 0 {
			// A step started has a position or deleted something; one not started does not count.
			if run.Position == nil && (len(run.Steps) == 0 || run.Steps[len(run.Steps)-1].Number != run.Reached) {
				run.Reached--
			}

			return nil
		}

		var next CleanupRun

		started := time.Now()

		err := database.update(func(writer *batchWriter) error {
			writer.counted = map[int]*CleanupCounts{}
			cursor := &cleanupCursor{batch: writer.batch, position: run.Position, need: need, always: step.always}
			finished, err := step.batch(writer, cursor)

			if err != nil {
				return err
			}

			next = withBatch(*run, step.name, writer.counted, -writer.batch.Counted(), time.Since(started))
			next.Position = cursor.position

			if finished {
				next.Reached, next.Position = next.Reached+1, nil
			}

			return database.cleanups.Save(writer.batch, next)
		})

		if err != nil {
			return err
		}

		*run = next

		select {
		case <-ctx.Done():
		case <-time.After(cleanupRest):
		}
	}

	return ctx.Err()
}

// withBatch returns a copy of the run with a batch of its current step added.
func withBatch(run CleanupRun, name string, counted map[int]*CleanupCounts, freed int64, took time.Duration) CleanupRun {
	steps := make([]CleanupStep, len(run.Steps))

	for index, step := range run.Steps {
		step.Projects = maps.Clone(step.Projects)

		for project, counts := range step.Projects {
			copied := *counts
			step.Projects[project] = &copied
		}

		steps[index] = step
	}

	run.Steps = steps

	if len(counted) == 0 {
		return run
	}

	if len(steps) == 0 || steps[len(steps)-1].Number != run.Reached {
		run.Steps = append(run.Steps, CleanupStep{Number: run.Reached, Name: name, Projects: map[int]*CleanupCounts{}})
	}

	step := &run.Steps[len(run.Steps)-1]
	step.Freed += freed
	step.Duration += took

	for project, counts := range counted {
		total := step.Projects[project]

		if total == nil {
			total = &CleanupCounts{}
			step.Projects[project] = total
		}

		total.Logs += counts.Logs
		total.Events += counts.Events
		total.Thinned += counts.Thinned
		total.Issues += counts.Issues
	}

	return run
}

// deleteLogs deletes logs of ranks, lowest level and oldest first, that
// arrived before the sequence bound returns (nil: any). Position: rank, sequence.
func (database *Store) deleteLogs(ranks []byte, bound func() uint64) cleanupBatch {
	return func(writer *batchWriter, cursor *cleanupCursor) (bool, error) {
		start, from := kv.Decode(cursor.position, 0).U8(), cursor.number(1)
		before := uint64(math.MaxUint64)

		if bound != nil {
			before = bound()
		}

		for _, rank := range ranks {
			if rank < start {
				continue
			}

			if rank > start {
				from = 0
			}

			full := false

			err := database.logs.Primary(kv.U8(rank)).From(kv.U64(from)).Before(kv.U64(before)).Each(database.db, func(entry kv.Entry[logRecord]) (bool, error) {
				if full = cursor.full(); full {
					return false, nil
				}

				database.logs.DeleteEntry(writer.batch, entry)
				writer.count(entry.Record.ProjectID).Logs++
				cursor.records++
				from = entry.Record.Sequence + 1

				return true, nil
			})

			if err != nil || full {
				cursor.position = kv.K(kv.U8(rank), kv.U64(from))

				return false, err
			}
		}

		return true, nil
	}
}

// deleteOldestLogs deletes logs of ranks oldest first, whatever their level. Position: sequence.
func (database *Store) deleteOldestLogs(ranks []byte) cleanupBatch {
	return func(writer *batchWriter, cursor *cleanupCursor) (bool, error) {
		from := cursor.number(0)
		queries := make([]kv.Query[logRecord], 0, len(ranks))

		for _, rank := range ranks {
			queries = append(queries, database.logs.Primary(kv.U8(rank)).From(kv.U64(from)))
		}

		full := false

		err := oldestFirst(database.db, queries, func(entry kv.Entry[logRecord]) bool {
			if full = cursor.full(); full {
				return false
			}

			database.logs.DeleteEntry(writer.batch, entry)
			writer.count(entry.Record.ProjectID).Logs++
			cursor.records++
			from = entry.Record.Sequence + 1

			return true
		})

		cursor.position = kv.K(kv.U64(from))

		return !full, err
	}
}

// eachIssue visits, by ID from the cursor on, the issues wants accepts as
// they are now; visit says whether it is done with one. Position: issue ID.
func (database *Store) eachIssue(wants func(*issueRecord) bool,
	visit func(writer *batchWriter, cursor *cleanupCursor, issue *issueRecord) (bool, error),
) cleanupBatch {
	return func(writer *batchWriter, cursor *cleanupCursor) (bool, error) {
		database.issuesMutex.RLock()
		last := database.table.lastID
		database.issuesMutex.RUnlock()

		for id := max(int64(cursor.number(0)), 1); id <= last; id++ {
			// The table changes only under the write lock, which this batch holds.
			database.issuesMutex.RLock()
			stored, ok := database.table.byID[id]
			database.issuesMutex.RUnlock()

			if !ok || !wants(stored) {
				continue
			}

			if cursor.full() {
				cursor.position = kv.K(kv.U64(uint64(id)))

				return false, nil
			}

			issue, _ := writer.issue(id)
			done, err := visit(writer, cursor, issue)

			if err != nil || !done {
				cursor.position = kv.K(kv.U64(uint64(id)))

				return false, err
			}
		}

		return true, nil
	}
}

// trimIssues deletes the oldest events received before cutoff (zero: any)
// beyond keep, of the issues match accepts (nil: all).
func (database *Store) trimIssues(keep int, cutoff time.Time, match func(*issueRecord) bool) cleanupBatch {
	wants := func(issue *issueRecord) bool { return issue.Stored > keep && (match == nil || match(issue)) }

	return database.eachIssue(wants, func(writer *batchWriter, cursor *cleanupCursor, issue *issueRecord) (bool, error) {
		done := true

		err := database.events.Primary(kv.U64(uint64(issue.ID))).Each(database.db, func(entry kv.Entry[eventRecord]) (bool, error) {
			if issue.Stored <= keep || (!cutoff.IsZero() && !entry.Record.ReceivedAt.Before(cutoff)) {
				return false, nil
			}

			if cursor.full() {
				done = false

				return false, nil
			}

			writer.deleteEvent(entry)

			cursor.records++

			return true, nil
		})

		return done, err
	})
}

// deleteOldestEvents deletes events received before cutoff (zero: any), oldest
// first whatever their level, but not the latest of an issue. Position: sequence.
func (database *Store) deleteOldestEvents(cutoff time.Time) cleanupBatch {
	return func(writer *batchWriter, cursor *cleanupCursor) (bool, error) {
		from := cursor.number(0)
		queries := make([]kv.Query[eventRecord], 0, len(allRanks))

		for _, rank := range allRanks {
			queries = append(queries, database.events.By("age", kv.U8(rank)).From(kv.U64(from)))
		}

		full := false

		err := oldestFirst(database.db, queries, func(entry kv.Entry[eventRecord]) bool {
			if !cutoff.IsZero() && !entry.Record.ReceivedAt.Before(cutoff) {
				return false
			}

			if full = cursor.full(); full {
				return false
			}

			from = entry.Record.Sequence + 1
			cursor.records++

			// The latest event of an issue is the one it was last seen with.
			if issue, ok := writer.issue(entry.Record.IssueID); ok && issue.Stored > 1 && entry.Record.ReceivedAt.Before(issue.LastSeen) {
				writer.deleteEvent(entry)
			}

			return true
		})

		cursor.position = kv.K(kv.U64(from))

		return !full, err
	}
}

// deleteUnseen deletes, longest unseen first, the events of the issues last
// seen before seenBefore (zero: any) or, with whole, the issues.
// Position: last seen and ID of the last issue done.
func (database *Store) deleteUnseen(seenBefore time.Time, whole bool) cleanupBatch {
	type unseen struct {
		lastSeen int64
		id       int64
	}

	compare := func(a, b unseen) int { return cmp.Or(cmp.Compare(a.lastSeen, b.lastSeen), cmp.Compare(a.id, b.id)) }

	return func(writer *batchWriter, cursor *cleanupCursor) (bool, error) {
		after := unseen{math.MinInt64, 0}

		if len(cursor.position) > 0 {
			after = unseen{int64(cursor.number(0)), int64(cursor.number(8))}
		}

		var issues []unseen

		database.issuesMutex.RLock()

		for _, issue := range database.table.byID {
			candidate := unseen{issue.LastSeen.UnixNano(), issue.ID}

			if (whole || issue.Stored > 0) && (seenBefore.IsZero() || issue.LastSeen.Before(seenBefore)) && compare(candidate, after) > 0 {
				issues = append(issues, candidate)
			}
		}

		database.issuesMutex.RUnlock()
		slices.SortFunc(issues, compare)

		for _, candidate := range issues {
			done := !cursor.full()

			if !done {
				cursor.position = kv.K(kv.U64(uint64(after.lastSeen)), kv.U64(uint64(after.id)))

				return false, nil
			}

			issue, _ := writer.issue(candidate.id)

			err := database.events.Primary(kv.U64(uint64(issue.ID))).Each(database.db, func(entry kv.Entry[eventRecord]) (bool, error) {
				if cursor.full() {
					done = false

					return false, nil
				}

				writer.deleteEvent(entry)

				cursor.records++

				return true, nil
			})

			if err != nil || !done {
				cursor.position = kv.K(kv.U64(uint64(after.lastSeen)), kv.U64(uint64(after.id)))

				return false, err
			}

			if whole {
				if err := writer.deleteIssue(issue); err != nil {
					return false, err
				}

				cursor.records++
			}

			after = candidate
		}

		return true, nil
	}
}

// count is the counts of a project in this batch; a batch outside the
// cleanup counts nothing.
func (writer *batchWriter) count(project int) *CleanupCounts {
	if writer.counted == nil {
		return &CleanupCounts{}
	}

	counts := writer.counted[project]

	if counts == nil {
		counts = &CleanupCounts{}
		writer.counted[project] = counts
	}

	return counts
}
