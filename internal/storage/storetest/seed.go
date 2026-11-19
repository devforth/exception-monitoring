package storetest

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/database"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// The seed: 60 days of a server with a size limit, for TestCleanupBench and TestCleanupUnderIngestLoad.

// SeedNow is when the seed ends; a copy is opened with its clock going on from there.
var SeedNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

const (
	seedVersion      = "3" // a seed of another version is built again
	seedDays         = 60
	seedSlot         = 10 * time.Minute
	seedSlotsPerDay  = int(24 * time.Hour / seedSlot)
	seedProjects     = 3
	seedLogsPerSlot  = 210 // at scale 1
	seedEventsPerDay = 3000
)

// OpenSeed opens a copy of the seed of scale (1 is about 1 GB) in
// base/run-<scale>, written to the disk before it opens, so the measurement
// does not pay for it. The seed is built in base/seed-<scale> the first
// time, and again for another seedVersion or a store that cannot open it.
// The clock returned is the store's, going on from SeedNow.
func OpenSeed(t testing.TB, base string, scale float64) (*database.Store, func() time.Time) {
	t.Helper()

	seed := filepath.Join(base, fmt.Sprintf("seed-%g", scale))
	copied := filepath.Join(base, fmt.Sprintf("run-%g", scale))

	for attempt := range 2 {
		if version, err := os.ReadFile(filepath.Join(seed, "done")); attempt > 0 || err != nil || string(version) != seedVersion {
			buildSeed(t, seed, scale)
		}

		started := time.Now()

		if err := os.RemoveAll(copied); err != nil {
			t.Fatal(err)
		}

		if err := os.CopyFS(copied, os.DirFS(seed)); err != nil {
			t.Fatal(err)
		}

		if err := syncTree(copied); err != nil {
			t.Fatal(err)
		}

		t.Logf("seed copied and written to the disk in %s", time.Since(started).Round(time.Millisecond))

		opened, err := database.Open(copied)

		if err != nil {
			if attempt == 0 {
				t.Logf("seed not opened, building it again: %v", err)

				continue
			}

			t.Fatal(err)
		}

		t.Cleanup(func() {
			_ = opened.Close()
			_ = os.RemoveAll(copied)
		})

		clock := seedClock(time.Now())

		opened.SetClock(clock)

		return opened, clock
	}

	return nil, nil
}

// seedClock goes on from SeedNow since started.
func seedClock(started time.Time) func() time.Time {
	return func() time.Time { return SeedNow.Add(time.Since(started)) }
}

// syncTree writes the files and directories under dir to the disk.
func syncTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		file, err := os.Open(path)

		if err != nil {
			return err
		}

		return errors.Join(file.Sync(), file.Close())
	})
}

// buildSeed writes the seed in a directory of its own and moves it to dir
// once complete, so a half-built seed is never taken for one.
func buildSeed(t testing.TB, dir string, scale float64) {
	t.Helper()

	started := time.Now()
	building := dir + ".building"

	if err := os.RemoveAll(building); err != nil {
		t.Fatal(err)
	}

	built, err := database.Open(building)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = built.Close() })

	ctx := context.Background()
	random := rand.New(rand.NewPCG(1, 2))
	start := SeedNow.Add(-seedDays * 24 * time.Hour)
	logsPerSlot := int(math.Round(seedLogsPerSlot * scale))
	eventsPerSlot := int(math.Round(seedEventsPerDay * scale / float64(seedSlotsPerDay)))
	issueIDs := map[int]string{}

	for slot := range seedDays * seedSlotsPerDay {
		at := start.Add(time.Duration(slot) * seedSlot)

		built.SetClock(func() time.Time { return at })

		envelope := database.Envelope{Logs: SeedLogs(random, at, logsPerSlot, slot)}
		day := slot / seedSlotsPerDay
		numbers := make([]int, 0, eventsPerSlot)

		for index := range eventsPerSlot {
			// Issues drift: each day starts 15 numbers later, so old issues stop being seen.
			number := day*15 + random.IntN(random.IntN(300)+1)
			numbers = append(numbers, number)
			envelope.Events = append(envelope.Events, SeedEvent(random, 1+number%seedProjects, fmt.Sprintf("issue-%d", number), at.Add(time.Duration(index)*time.Second)))
		}

		results, err := built.Write([]database.Envelope{envelope})

		if err != nil {
			t.Fatal(err)
		}

		for index, result := range results[0] {
			issueIDs[numbers[index]] = result.IssueID
		}

		// The server thins events as they turn a month old; once a day is enough here.
		if (slot+1)%seedSlotsPerDay == 0 {
			if _, err := built.ReduceAging(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	built.SetClock(func() time.Time { return SeedNow })

	// Of every 20 issues by number, 4 are resolved and 1 ignored.
	statuses := map[string][]string{}

	for _, number := range slices.Sorted(maps.Keys(issueIDs)) {
		switch number % 20 {
		case 0, 1, 2, 3:
			statuses[store.StatusResolved] = append(statuses[store.StatusResolved], issueIDs[number])
		case 4:
			statuses[store.StatusIgnored] = append(statuses[store.StatusIgnored], issueIDs[number])
		}
	}

	for status, ids := range statuses {
		for chunk := range slices.Chunk(ids, 100) {
			if _, err := built.SetIssuesStatus(ctx, chunk, status, store.Actor{Name: "seed", Source: store.SourceAPI}); err != nil {
				t.Fatal(err)
			}
		}
	}

	size, err := built.DatabaseSize(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if err := built.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(building, "done"), []byte(seedVersion), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(building, dir); err != nil {
		t.Fatal(err)
	}

	t.Logf("seed of scale %g: %d issues, %s on disk, built in %s", scale, len(issueIDs), config.FormatSize(size), time.Since(started).Round(time.Second))
}

var seedLevels = []string{"info", "info", "info", "info", "info", "info", "info", "info", "info", "info",
	"debug", "debug", "debug", "debug", "warn", "warn", "error", "error", "error", "fatal"}

// SeedLogs are count logs of the seed's projects at its mix of levels, each
// a message of its own (the server caps only repeated ones).
func SeedLogs(random *rand.Rand, at time.Time, count, salt int) []store.NewLog {
	logs := make([]store.NewLog, 0, count)

	for index := range count {
		logs = append(logs, store.NewLog{ProjectID: 1 + random.IntN(seedProjects), Timestamp: at, Level: seedLevels[random.IntN(len(seedLevels))],
			Body: fmt.Sprintf("request %d-%d handled: %s", salt, index, seedText(random, 160)), TraceID: seedText(random, 32),
			SpanID: seedText(random, 16), Fingerprint: seedText(random, 16)})
	}

	return logs
}

// SeedEvent is an error of about 2.5 KB with every part thinning drops.
func SeedEvent(random *rand.Rand, project int, fingerprint string, at time.Time) store.NewEvent {
	payload := fmt.Sprintf(`{"level":"error","platform":"node","exception":{"values":[{"type":"Error","value":"%s",`+
		`"stacktrace":{"frames":[{"filename":"/app/server.js","function":"handler","lineno":10,"in_app":true,"context_line":"throw error","vars":{"state":"%s"}}]}}]},`+
		`"breadcrumbs":{"values":[{"category":"http","message":"%s"},{"category":"log","message":"%s"}]},"request":{"method":"GET","url":"http://app/%s"},`+
		`"tags":{"release":"1.0"},"contexts":{"runtime":{"name":"node"},"state":{"dump":"%s"}}}`,
		fingerprint, seedText(random, 200), seedText(random, 300), seedText(random, 300), seedText(random, 40), seedText(random, 800))

	return store.NewEvent{ProjectID: project, EventID: seedText(random, 32), Fingerprint: fingerprint, Title: "Error: " + fingerprint,
		Level: "error", Timestamp: at, ReceivedAt: at, Payload: []byte(payload), TraceID: seedText(random, 32), SpanID: seedText(random, 16)}
}

// seedText is random hex, which does not compress: the files make the size.
func seedText(random *rand.Rand, length int) string {
	raw := make([]byte, length/2)

	for index := range raw {
		raw[index] = byte(random.Uint32())
	}

	return hex.EncodeToString(raw)
}

// Percentile of sorted durations, by the nearest rank.
func Percentile(sorted []time.Duration, percent int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	return sorted[max(0, min(len(sorted)-1, (len(sorted)*percent+99)/100-1))]
}
