package database_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/database"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/storage/storetest"
)

// The cleanup on a large database (storetest.OpenSeed, scale 1), measured only on request:
// CLEANUP_BENCH_DIR=$PWD/data/bench-cleanup go test ./internal/storage/database -run TestCleanupBench -v -timeout 2h
// (-run TestCleanupBench/deep for one scenario). The first run builds the seed,
// which takes a few minutes. Use a directory on a disk: on tmpfs (/tmp on many
// systems) a sync costs nothing.
func TestCleanupBench(t *testing.T) {
	base := os.Getenv("CLEANUP_BENCH_DIR")

	if base == "" {
		t.Skip("set CLEANUP_BENCH_DIR")
	}

	for _, scenario := range []struct {
		name  string
		limit float64 // of the size on disk
	}{{"always", 0.95}, {"half", 0.5}, {"deep", 0.15}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := storetest.OpenSeed(t, base, 1)
			size, err := db.DatabaseSize(ctx)

			if err != nil {
				t.Fatal(err)
			}

			limit := int64(float64(size) * scenario.limit)
			probeCtx, stop := context.WithCancel(ctx)
			probed := benchProbe(probeCtx, db)
			started := time.Now()
			run, err := db.Cleanup(ctx, limit)
			elapsed := time.Since(started)

			stop()

			probe := <-probed

			if err != nil || probe.err != nil {
				t.Fatal(err, probe.err)
			}

			var stepsTime time.Duration

			for _, step := range run.Steps {
				var total store.CleanupCounts

				for _, counts := range step.Projects {
					total.Add(*counts)
				}

				stepsTime += step.Duration
				t.Logf("step %d, %s: freed %s in %s (%s)", step.Number, step.Name, config.FormatSize(step.Freed), step.Duration.Round(time.Millisecond), total)
			}

			waits := probe.waits
			slices.Sort(waits)
			t.Logf("limit %s, size %s -> %s, reached step %d, total %s: steps %s, writes, syncs, rests and compaction %s; ingest batches %d, p50 %s, p99 %s, max %s",
				config.FormatSize(limit), config.FormatSize(run.SizeBefore), config.FormatSize(run.SizeAfter), run.Reached,
				elapsed.Round(time.Millisecond), stepsTime.Round(time.Millisecond), (elapsed - stepsTime).Round(time.Millisecond),
				len(waits), storetest.Percentile(waits, 50), storetest.Percentile(waits, 99), storetest.Percentile(waits, 100))

			if run.Reached == 0 || len(run.Steps) == 0 || run.SizeAfter >= limit {
				t.Errorf("the cleanup deleted nothing or stayed over the limit: %+v", run)
			}

			if problems, err := db.Verify(); err != nil || len(problems) > 0 {
				t.Errorf("verify: %v %v", problems, err)
			}
		})
	}
}

type probeResult struct {
	waits []time.Duration
	err   error
}

// benchProbe writes an ingest-sized batch every 20 ms and reports how long each took.
func benchProbe(ctx context.Context, db *database.Store) <-chan probeResult {
	result := make(chan probeResult, 1)

	go func() {
		var (
			probe probeResult
			index int
		)

		random := rand.New(rand.NewPCG(7, 7))

		for ctx.Err() == nil {
			envelope := database.Envelope{Events: []store.NewEvent{storetest.SeedEvent(random, 1, fmt.Sprintf("probe-%d", index%5), storetest.SeedNow)},
				Logs: storetest.SeedLogs(random, storetest.SeedNow, 10, index)}
			started := time.Now()

			if _, probe.err = db.Write([]database.Envelope{envelope}); probe.err != nil {
				break
			}

			probe.waits = append(probe.waits, time.Since(started))
			index++

			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Millisecond):
			}
		}

		result <- probe
	}()

	return result
}
