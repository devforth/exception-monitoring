package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/platform/background"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/logging"
	"github.com/devforth/exception-monitoring/internal/storage/database"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/storage/storetest"
)

// The peak the server is measured at, only on request:
// INGEST_LOAD=1 INGEST_DIR=$PWD/data go test ./internal/ingest -run 'TestIngestLoad$' -v -timeout 1h
// CLEANUP_BENCH_DIR=$PWD/data/bench-cleanup go test ./internal/ingest -run TestCleanupUnderIngestLoad -v -timeout 2h
// Both run the server's ingest and cleanup loop under its memory limit, with
// the rate limit off: the default (1000 envelopes a DSN a minute) would answer
// 429 to the logs that follow the 1000 errors.

// load is the peak: projects DSNs at once, each sending events errors and logs envelopes of 11 logs.
type load struct{ projects, events, logs int }

// targetLoad is 50 DSNs, each with 1000 errors and 200 log envelopes;
// INGEST_SCALE=100 sends 100 errors and 20 log envelopes a DSN instead.
func targetLoad() load {
	shape := load{projects: 50, events: 1000, logs: 200}

	if scale := os.Getenv("INGEST_SCALE"); scale != "" {
		_, _ = fmt.Sscan(scale, &shape.events)
		shape.logs = max(1, shape.events/5)
	}

	return shape
}

// logsPerMessage is the cap on repeated log messages: INGEST_REPEATS=0 turns
// it off to measure the write path alone (the load repeats the same 11 lines,
// so with the cap nearly every log is deleted as soon as it is written).
func logsPerMessage() int {
	if value := os.Getenv("INGEST_REPEATS"); value != "" {
		limit, _ := strconv.Atoi(value)

		return limit
	}

	return config.LogsPerMessage
}

// loadServer runs ingest and the cleanup loop on database as main does, until the test ends.
func loadServer(t *testing.T, db *database.Store, projects config.Projects, clock func() time.Time, limit int64) (*Handler, *Pipeline) {
	t.Helper()

	logger := logging.New(os.Stderr, logging.FormatPretty, slog.LevelWarn)
	settings := &config.Config{Projects: projects, AllowedOrigins: []string{"*"}}
	handler := New(logger, settings, projects, nil)
	handler.now = clock
	pipeline := NewPipeline(handler, db, StoreSink{Logger: logger})
	pipeline.LogsPerMessage = logsPerMessage()
	ctx, stop := context.WithCancel(context.Background())
	piped, cleaned := make(chan struct{}), make(chan struct{})

	go func() { defer close(piped); pipeline.Run(ctx) }()

	// The database lane of cmd/exception-monitoring/jobs.go.
	jobsLogger := logging.New(os.Stderr, logging.FormatPretty, slog.LevelInfo)
	stopJobs := background.Start(ctx, jobsLogger, background.Lane{Name: "database", Jobs: []background.Job{
		{Name: "inbox compaction", AtStart: true, First: config.InboxCompactionAt, Every: config.CleanupInterval, Timeout: config.InboxCompactionTimeout,
			Run: db.CompactInbox},
		{Name: "size check", AtStart: true, First: config.CleanupInterval, Every: config.CleanupInterval, Timeout: config.SizeCheckTimeout,
			Wake: db.CleanupWake(), Run: func(ctx context.Context) error { return db.CheckSize(ctx, limit, jobsLogger) }},
	}})

	go func() {
		defer close(cleaned)

		<-ctx.Done()
		stopJobs()
	}()

	// Before the store closes (cleanups run last added first).
	t.Cleanup(func() {
		stop()
		<-piped
		<-cleaned
	})

	return handler, pipeline
}

// sendLoad sends the events, then the logs of shape. With settle each burst
// is watched until the process is idle again, so the compaction it causes is
// counted with it.
func sendLoad(t *testing.T, handler http.Handler, pipeline *Pipeline, projects config.Projects, shape load, label string, settle bool) []burst {
	t.Helper()

	event, logs := fixtures.Envelope(t, "node-event-unhandled"), fixtures.Envelope(t, "node-logs")
	bursts := []burst{sendBurst(t, "events", "events"+label, handler, projects, shape.events, event, 20, pipeline.Stored, settle)}
	before := pipeline.Stored()

	return append(bursts, sendBurst(t, "logs", "logs, repeats capped"+label, handler, projects, shape.logs, logs, 0, func() int64 { return pipeline.Stored() - before }, settle))
}

// The ingest goal: 50 DSNs sending 1000 errors each at once, through the
// inbox, the key-value store and the cleanup loop, as on a server with an
// empty database (see the commands above). The database is in a temporary
// directory, on tmpfs on many systems, where a disk sync costs nothing:
// INGEST_DIR puts it on a disk.
func TestIngestLoad(t *testing.T) {
	if os.Getenv("INGEST_LOAD") == "" {
		t.Skip("set INGEST_LOAD=1")
	}

	config.LimitMemory()

	dir := t.TempDir()

	if parent := os.Getenv("INGEST_DIR"); parent != "" {
		var err error

		if dir, err = os.MkdirTemp(parent, "ingest-load-"); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}

	db, err := database.Open(dir)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	shape := targetLoad()
	projects := benchProjects(shape.projects)

	// What the in-process load generator alone costs: the same requests to a handler that only reads them.
	generator := sendBurst(t, "generator", "load generator alone (events)", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		w.WriteHeader(http.StatusOK)
	}), projects, shape.events, fixtures.Envelope(t, "node-event-unhandled"), 20, func() int64 { return math.MaxInt64 }, true)

	handler, pipeline := loadServer(t, db, projects, time.Now, config.DefaultMaxDatabaseBytes)
	reports := []string{generator.String()}

	for _, done := range sendLoad(t, handler, pipeline, projects, shape, " (rate limit off)", true) {
		reports = append(reports, done.String())
	}

	ctx := context.Background()
	logs, more, err := db.CountLogs(ctx, store.LogFilter{})

	if err != nil {
		t.Fatal(err)
	}

	size, err := db.DatabaseSize(ctx)

	if err != nil {
		t.Fatal(err)
	}

	var memory runtime.MemStats

	runtime.ReadMemStats(&memory)

	reports = append(reports, fmt.Sprintf("database %s on disk, %d logs kept (more: %v), Go heap %d MB", config.FormatSize(size), logs, more, memory.HeapAlloc>>20))

	for _, report := range reports {
		t.Log(report)
	}
}

// cleanupSeedScale puts the seed a little under MAX_DATABASE_SIZE's default;
// the limit is set just under the seed, so the cleanup starts with the peak.
const cleanupSeedScale = 1.79 // about 98% of 2 GB

// The peak of TestIngestLoad when the database is at MAX_DATABASE_SIZE (2
// GB): the cleanup starts during it and deletes while events and logs arrive
// (see the commands above). The first run builds the seed, which takes a few minutes.
func TestCleanupUnderIngestLoad(t *testing.T) {
	base := os.Getenv("CLEANUP_BENCH_DIR")

	if base == "" {
		t.Skip("set CLEANUP_BENCH_DIR")
	}

	config.LimitMemory()

	ctx := context.Background()
	db, clock := storetest.OpenSeed(t, base, cleanupSeedScale)
	size, err := db.DatabaseSize(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if share := float64(size) / config.DefaultMaxDatabaseBytes; share < 0.9 || share > 1.05 {
		t.Fatalf("the seed is %s, %.0f%% of the default limit: change cleanupSeedScale", config.FormatSize(size), share*100)
	}

	// At the first check, as when a server at the limit gets a peak.
	limit := size * 99 / 100

	shape := targetLoad()
	projects := benchProjects(shape.projects)
	whole := startWatch()
	loadStart, storeStart := time.Now(), clock()
	handler, pipeline := loadServer(t, db, projects, clock, limit)
	timeline := watchCleanups(t, db, loadStart)
	bursts := sendLoad(t, handler, pipeline, projects, shape, " (rate limit off), cleanup running", false)
	loaded := time.Since(loadStart)

	// The cleanup goes on after the load; a run shows once its first compaction is done.
	var runs []store.CleanupRun

	for deadline := time.Now().Add(30 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if runs, err = db.CleanupHistory(ctx, 10); err != nil {
			t.Fatal(err)
		}

		if len(runs) > 0 && !slices.ContainsFunc(runs, func(run store.CleanupRun) bool { return run.Status == store.CleanupRunning }) {
			break
		}

		if len(runs) == 0 && time.Since(loadStart) > loaded+2*time.Minute {
			size, _ := db.DatabaseSize(ctx)
			t.Fatalf("no cleanup started: %s on disk, limit %s", config.FormatSize(size), config.FormatSize(limit))
		}

		if time.Now().After(deadline) {
			t.Fatalf("the cleanup did not finish: %+v", runs)
		}
	}

	used := whole.settle()
	seen := timeline()

	for _, done := range bursts {
		t.Log(done)
	}

	t.Logf("load stored %s after it began", loaded.Round(time.Millisecond))

	deleted := false

	for _, run := range slices.Backward(runs) {
		at := func(since time.Duration) string {
			if since == 0 {
				return "never seen"
			}

			return "+" + since.Round(100*time.Millisecond).String()
		}

		failure := ""

		if run.Error != "" {
			failure = ", error: " + run.Error
		}

		t.Logf("cleanup %s: began +%s, compacted and saved %s, deleting from %s, done +%s; %s -> %s on disk (compacted %s), reached step %d%s",
			run.Status, run.StartedAt.Sub(storeStart).Round(100*time.Millisecond), at(seen[run.Sequence].saved), at(seen[run.Sequence].deleting),
			run.StartedAt.Add(run.Duration).Sub(storeStart).Round(100*time.Millisecond),
			config.FormatSize(run.SizeBefore), config.FormatSize(run.SizeAfter), config.FormatSize(run.Compacted), run.Reached, failure)

		if run.Status != store.CleanupDone {
			t.Errorf("cleanup %s%s", run.Status, failure)
		}

		deleted = deleted || (run.Reached > 0 && run.SizeAfter < limit)
	}

	t.Logf("whole run until settled: %s | memory peak %d MB | %s", used.elapsed.Round(time.Millisecond), used.peak, used)

	if !deleted {
		t.Error("no cleanup deleted anything and ended under the limit")
	}

	if pending, err := db.Pending(0, 10, nil); err != nil || len(pending) > 0 {
		t.Errorf("inbox not empty: %d entries, %v", len(pending), err)
	}

	if problems, err := db.Verify(); err != nil || len(problems) > 0 {
		t.Errorf("verify: %v %v", problems, err)
	}
}

// cleanupSeen is when a run showed in the history (its start compaction
// done) and when its first deletes were saved, since the load began.
type cleanupSeen struct{ saved, deleting time.Duration }

// watchCleanups polls the cleanup history until the returned function is called, which gives what it saw.
func watchCleanups(t *testing.T, db *database.Store, since time.Time) func() map[uint64]cleanupSeen {
	t.Helper()

	seen := map[uint64]cleanupSeen{}
	stop, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(50 * time.Millisecond)

		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}

			runs, err := db.CleanupHistory(context.Background(), 10)

			if err != nil {
				continue
			}

			for _, run := range runs {
				entry, known := seen[run.Sequence]

				if !known {
					entry.saved = time.Since(since)
				}

				if entry.deleting == 0 && len(run.Steps) > 0 {
					entry.deleting = time.Since(since)
				}

				seen[run.Sequence] = entry
			}
		}
	}()

	return func() map[uint64]cleanupSeen {
		close(stop)
		<-done

		return seen
	}
}

// currentRSS is the memory the process holds now (resident set), in MB.
func currentRSS() int64 {
	status, err := os.ReadFile("/proc/self/status")

	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(status), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "VmRSS:" {
			var kilobytes int64

			_, _ = fmt.Sscan(fields[1], &kilobytes)

			return kilobytes >> 10
		}
	}

	return 0
}

// diskWritten is what the process has sent to the disk, in bytes (nothing on tmpfs).
func diskWritten() int64 {
	io, err := os.ReadFile("/proc/self/io")

	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(io), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "write_bytes:" {
			written, _ := strconv.ParseInt(fields[1], 10, 64)

			return written
		}
	}

	return 0
}

// watch samples the process: its peak memory, and its CPU, busy cores at the
// peak over 250 ms and on average.
type watch struct {
	started    time.Time
	cpu        time.Duration
	written    int64
	stop, done chan struct{}
	peak       int64 // MB
	peakCores  float64
}

func startWatch() *watch {
	watching := &watch{started: time.Now(), cpu: cpuTime(), written: diskWritten(), stop: make(chan struct{}), done: make(chan struct{}), peak: currentRSS()}

	go func() {
		defer close(watching.done)

		ticker := time.NewTicker(20 * time.Millisecond)
		lastCPU, lastAt := watching.cpu, watching.started

		defer ticker.Stop()

		for {
			select {
			case <-watching.stop:
				return
			case <-ticker.C:
			}

			watching.peak = max(watching.peak, currentRSS())

			// The CPU and the clock are read together: the ticker's time is when the tick was due.
			if cpu, at := cpuTime(), time.Now(); at.Sub(lastAt) >= 250*time.Millisecond {
				watching.peakCores = max(watching.peakCores, float64(cpu-lastCPU)/float64(at.Sub(lastAt)))
				lastCPU, lastAt = cpu, at
			}
		}
	}()

	return watching
}

// usage is what a watch saw.
type usage struct {
	elapsed, cpu time.Duration
	written      int64
	peak         int64 // MB
	peakCores    float64
}

func (watching *watch) end() usage {
	close(watching.stop)
	<-watching.done

	return usage{elapsed: time.Since(watching.started), cpu: cpuTime() - watching.cpu, written: diskWritten() - watching.written,
		peak: watching.peak, peakCores: watching.peakCores}
}

// settle ends the watch once the process is idle: under 0.2 cores over 500 ms
// and nothing written to the disk for a second (2 minutes at most).
func (watching *watch) settle() usage {
	type sample struct {
		at  time.Time
		cpu time.Duration
	}

	var samples []sample

	written, changed := diskWritten(), time.Now()

	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		now := sample{time.Now(), cpuTime()}
		samples = append(samples, now)

		if current := diskWritten(); current != written {
			written, changed = current, now.at
		}

		index := slices.IndexFunc(samples, func(earlier sample) bool { return now.at.Sub(earlier.at) <= 500*time.Millisecond })

		if index > 0 {
			earlier := samples[index-1]

			if float64(now.cpu-earlier.cpu)/float64(now.at.Sub(earlier.at)) < 0.2 && now.at.Sub(changed) >= time.Second {
				break
			}
		}
	}

	return watching.end()
}

func (used usage) String() string {
	disk := fmt.Sprintf("disk %d MB written", used.written>>20)

	if used.written == 0 {
		disk = "nothing written to a disk (tmpfs?)"
	}

	return fmt.Sprintf("CPU %.1f s: %.1f cores on average, %.1f at the peak (of %d) | %s",
		used.cpu.Seconds(), used.cpu.Seconds()/used.elapsed.Seconds(), used.peakCores, runtime.NumCPU(), disk)
}

// burst is the report of one load run.
type burst struct {
	name      string
	requests  int
	accepted  time.Duration // until every request was answered
	stored    time.Duration // until everything was stored
	latencies []time.Duration
	rejected  int
	// the memory of the process before the burst, and after it once the
	// garbage is collected and returned
	before, after int64
	used          usage // until stored, or settled
}

func (result burst) String() string {
	if len(result.latencies) == 0 {
		return result.name + ": no requests"
	}

	slices.Sort(result.latencies)

	at := func(percent int) time.Duration {
		return storetest.Percentile(result.latencies, percent).Round(time.Microsecond)
	}

	return fmt.Sprintf("%-50s %6d requests | answered %6.2fs, stored %6.2fs = %6.0f/s, watched %6.2fs | answer p50 %8v p99 %8v max %8v | rejected %d | memory %d MB before, %d peak, %d after | %s",
		result.name, result.requests, result.accepted.Seconds(), result.stored.Seconds(), float64(result.requests)/result.stored.Seconds(), result.used.elapsed.Seconds(),
		at(50), at(99), at(100), result.rejected, result.before, result.used.peak, result.after, result.used)
}

// sendBurst sends perProject envelopes from every project at once (one
// sender per project) and waits until stored reports them all, then, with
// settle, until the process is idle.
func sendBurst(t *testing.T, kind, name string, handler http.Handler, projects config.Projects, perProject int, envelope []byte, issues int, stored func() int64, settle bool) burst {
	t.Helper()

	result := burst{name: name, requests: len(projects) * perProject, before: currentRSS()}
	watching := startWatch()

	// INGEST_HEAP=file writes a heap profile at the burst's largest live heap, to file.<kind>.
	if path := os.Getenv("INGEST_HEAP"); path != "" {
		stop := make(chan struct{})

		defer close(stop)

		go func() {
			var peak uint64

			for {
				select {
				case <-stop:
					t.Logf("%s: peak live heap %d MB", name, peak>>20)

					return
				case <-time.After(100 * time.Millisecond):
				}

				var memory runtime.MemStats

				runtime.ReadMemStats(&memory)

				if memory.HeapAlloc > peak+(32<<20) {
					peak = memory.HeapAlloc

					if file, err := os.Create(filepath.Clean(path + "." + kind)); err == nil {
						_ = pprof.WriteHeapProfile(file)
						_ = file.Close()
					}
				}
			}
		}()
	}

	var (
		mutex sync.Mutex
		group sync.WaitGroup
	)

	start := time.Now()

	for _, project := range projects {
		group.Add(1)

		go func() {
			defer group.Done()

			var latencies []time.Duration

			rejected := 0
			target := fmt.Sprintf("/api/%d/envelope/?sentry_key=%s", project.ID, project.PublicKey)

			for index := range perProject {
				issue := -1

				if issues > 0 {
					issue = index % issues
				}

				request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(varied(envelope, issue)))
				request.SetPathValue("projectID", fmt.Sprint(project.ID))

				response := httptest.NewRecorder()
				began := time.Now()

				handler.ServeHTTP(response, request)

				latencies = append(latencies, time.Since(began))

				if response.Code != http.StatusOK {
					rejected++
				}
			}

			mutex.Lock()

			result.latencies = append(result.latencies, latencies...)
			result.rejected += rejected
			mutex.Unlock()
		}()
	}

	group.Wait()

	result.accepted = time.Since(start)

	for deadline := time.Now().Add(10 * time.Minute); stored() < int64(result.requests-result.rejected); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: stored %d of %d", name, stored(), result.requests-result.rejected)
		}
	}

	result.stored = time.Since(start)

	if settle {
		result.used = watching.settle()
	} else {
		result.used = watching.end()
	}

	debug.FreeOSMemory()

	result.after = currentRSS()

	return result
}
