// Package background runs the server's periodic jobs on lanes: the jobs of a
// lane run one at a time, lanes side by side.
package background

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Job is one periodic pass.
type Job struct {
	Name    string
	AtStart bool          // a pass when the lane starts
	First   time.Duration // the first scheduled pass, after the start
	Every   time.Duration
	Wake    <-chan struct{} // passes between the scheduled ones, which stay where they are
	Note    string          // what wakes it, for Describe
	// Timeout ends a pass's ctx; a job that stops at it gets its next pass
	// soon after the others. One that does not stop is left running, and the
	// lane goes on without it, skipping its passes until it returns.
	Timeout time.Duration
	Run     func(ctx context.Context) error
}

// Describe reads the schedule: "at start, at 5m0s then every 10m0s, after 50 MB written".
func (job Job) Describe() string {
	var parts []string

	if job.AtStart {
		parts = append(parts, "at start")
	}

	parts = append(parts, fmt.Sprintf("at %s then every %s", job.First, job.Every))

	if job.Note != "" {
		parts = append(parts, job.Note)
	}

	return strings.Join(parts, ", ")
}

// Lane is a name and the jobs that share it.
type Lane struct {
	Name string
	Jobs []Job
}

// timeoutGrace is how long a pass past its Timeout may take to stop before the lane goes on.
var timeoutGrace = time.Second

// Run runs the lane's passes until ctx ends; a failed pass is logged.
func (lane Lane) Run(ctx context.Context, logger *slog.Logger) {
	next := make([]time.Time, len(lane.Jobs))
	running := make([]chan struct{}, len(lane.Jobs)) // a pass left running

	run := func(index int) {
		job := lane.Jobs[index]

		if running[index] != nil {
			select {
			case <-running[index]:
				running[index] = nil
			default:
				logger.Warn("background job skipped: its last pass still runs", "lane", lane.Name, "job", job.Name)

				return
			}
		}

		passCtx, cancel := ctx, context.CancelFunc(func() {})

		if job.Timeout > 0 {
			passCtx, cancel = context.WithTimeout(ctx, job.Timeout)
		}

		done := make(chan struct{})

		var err error

		go func() {
			defer close(done)
			defer cancel()

			err = job.Run(passCtx)
		}()

		var limit <-chan time.Time

		if job.Timeout > 0 {
			limit = time.After(job.Timeout + timeoutGrace)
		}

		select {
		case <-done:
		case <-limit:
			running[index] = done

			logger.Error("background job still runs past its time limit; the lane goes on", "lane", lane.Name, "job", job.Name, "limit", job.Timeout)

			return
		}

		switch {
		case ctx.Err() != nil:
		case errors.Is(err, context.DeadlineExceeded) || (err == nil && passCtx.Err() != nil):
			next[index] = time.Now() // goes on soon, after the passes due before it
		case err != nil:
			logger.Error("background job failed", "lane", lane.Name, "job", job.Name, "err", err)
		}
	}

	for index, job := range lane.Jobs {
		if job.AtStart && ctx.Err() == nil {
			run(index)
		}
	}

	started := time.Now()
	woken := make(chan int, len(lane.Jobs))

	for index, job := range lane.Jobs {
		if next[index].IsZero() {
			next[index] = started.Add(job.First)
		}

		if job.Wake != nil {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-job.Wake:
						select {
						case woken <- index:
						default: // a pass is already waiting
						}
					}
				}
			}()
		}
	}

	timer := time.NewTimer(0)

	defer timer.Stop()

	for ctx.Err() == nil {
		soonest := 0

		for index := range next {
			if next[index].Before(next[soonest]) {
				soonest = index
			}
		}

		timer.Reset(time.Until(next[soonest]))

		select {
		case <-ctx.Done():
			return
		case index := <-woken:
			run(index)
		case now := <-timer.C:
			for index, job := range lane.Jobs {
				if next[index].After(now) {
					continue
				}

				scheduled := next[index]

				run(index)

				if !next[index].Equal(scheduled) {
					continue // stopped at its time limit: again soon
				}

				for !next[index].After(time.Now()) {
					next[index] = next[index].Add(job.Every)
				}
			}
		}
	}
}

// Start runs every lane in a goroutine of its own; the returned function stops them and waits.
func Start(ctx context.Context, logger *slog.Logger, lanes ...Lane) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}, len(lanes))

	for _, lane := range lanes {
		go func() {
			defer func() { done <- struct{}{} }()

			lane.Run(ctx, logger)
		}()
	}

	return func() {
		cancel()

		for range lanes {
			<-done
		}
	}
}

// Describe lists every lane and job with its schedule, for the startup record.
func Describe(lanes ...Lane) []any {
	var fields []any

	for _, lane := range lanes {
		for _, job := range lane.Jobs {
			fields = append(fields, lane.Name+" › "+job.Name, job.Describe())
		}
	}

	return fields
}
