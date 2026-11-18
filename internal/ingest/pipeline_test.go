package ingest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/sqlite"

	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/kvstore"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// loadTestData reads a recorded envelope of testdata/envelopes.
func loadTestData(t testing.TB, name string) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "envelopes", name))

	if err != nil {
		t.Fatal(err)
	}

	return body
}

// varied returns envelope with a new event id and, for issue >= 0, an error
// type that makes it that issue (the type is part of the grouping).
func varied(envelope []byte, issue int) []byte {
	header := string(envelope[:bytes.IndexByte(envelope, '\n')])
	start := strings.Index(header, `"event_id":"`)
	id := make([]byte, 16)
	_, _ = rand.Read(id)

	if start >= 0 {
		envelope = bytes.ReplaceAll(envelope, []byte(header[start+12:start+44]), []byte(hex.EncodeToString(id)))
	}

	if issue >= 0 {
		envelope = bytes.Replace(envelope, []byte(`"type":"Error"`), fmt.Appendf(nil, `"type":"LoadTest%dError"`, issue), 1)
	}

	return envelope
}

func benchProjects(count int) config.Projects {
	var projects config.Projects

	for id := 1; id <= count; id++ {
		projects = append(projects, config.Project{ID: id, Name: fmt.Sprintf("project-%d", id), PublicKey: fmt.Sprintf("key%029d", id)})
	}

	return projects
}

func TestPipelineStoresEnvelopes(t *testing.T) {
	database, err := kvstore.Open(t.TempDir(), nil)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = database.Close() })

	settings := &config.Config{Projects: benchProjects(1), AllowedOrigins: []string{"*"}}
	handler := New(slog.New(slog.DiscardHandler), settings, settings.Projects, nil)
	notifier := &recordingNotifier{}
	pipeline := NewPipeline(handler, database, StoreSink{Logger: slog.New(slog.DiscardHandler), Notifier: notifier, Retention: config.Retention{EventsPerIssue: 3, LogsPerMessage: 2}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { defer close(done); pipeline.Run(ctx) }()

	event, logs := loadTestData(t, "node-event-unhandled.envelope"), loadTestData(t, "node-logs.envelope")
	target := "/api/1/envelope/?sentry_key=" + settings.Projects[0].PublicKey
	send := func(body []byte) {
		request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		request.SetPathValue("projectID", "1")

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("%d %s", response.Code, response.Body)
		}
	}

	waitStored := func(count int64) {
		t.Helper()

		for deadline := time.Now().Add(10 * time.Second); pipeline.Stored() < count; {
			if time.Now().After(deadline) {
				t.Fatalf("stored %d of %d", pipeline.Stored(), count)
			}

			time.Sleep(10 * time.Millisecond)
		}
	}

	// A retry of the SDK is recognized while its event is stored. The two
	// copies go first and one at a time: processors run in parallel, and a
	// copy trimmed away (3 events per issue) would no longer be recognized.
	duplicate := varied(event, 0)
	send(duplicate)
	waitStored(1)
	send(duplicate)
	waitStored(2)

	for range 5 {
		send(varied(event, 0))
	}

	send(varied(event, 1))
	send(logs)
	send(logs)
	send(logs)

	waitStored(11)

	// A resolved issue that happens again is notified as a regression.
	issues, err := database.ListIssues(context.Background(), store.IssueFilter{Sort: "event_count", Limit: 1})

	if err != nil || len(issues) != 1 {
		t.Fatal(issues, err)
	}

	if _, err := database.SetIssuesStatus(context.Background(), []int64{issues[0].ID}, store.StatusResolved, store.Actor{Source: store.SourceAPI}); err != nil {
		t.Fatal(err)
	}

	send(varied(event, 0))
	waitStored(12)
	cancel()
	<-done

	kinds := map[notify.Kind]int{}

	for _, notification := range notifier.notifications {
		kinds[notification.Kind]++
	}

	if len(notifier.notifications) != 3 || kinds[notify.NewIssue] != 2 || kinds[notify.Regression] != 1 {
		t.Errorf("notifications: %v", kinds)
	}

	counts := map[int]int{}
	issues, err = database.ListIssues(context.Background(), store.IssueFilter{})

	if err != nil {
		t.Fatal(err)
	}

	for _, issue := range issues {
		counts[issue.EventCount]++

		if events, _ := database.ListEvents(context.Background(), store.EventFilter{IssueID: issue.ID}); len(events) > 3 {
			t.Errorf("issue %d keeps %d events", issue.ID, len(events))
		}
	}

	// 7 distinct events of one issue (a retry is not counted), 1 of another.
	if len(counts) != 2 || counts[7] != 1 || counts[1] != 1 {
		t.Errorf("event counts: %v", counts)
	}

	if database.InboxBytes() != 0 {
		t.Errorf("inbox holds %d bytes", database.InboxBytes())
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

// burst is the report of one load run.
type burst struct {
	name      string
	requests  int
	accepted  time.Duration // until every request was answered
	stored    time.Duration // until everything was stored
	latencies []time.Duration
	rejected  int
	// memory of the process: before the burst, its peak, and after it once
	// the garbage is collected and returned
	before, peak, after int64
}

func (result burst) String() string {
	slices.Sort(result.latencies)

	at := func(q float64) time.Duration {
		return result.latencies[int(q*float64(len(result.latencies)-1))].Round(time.Microsecond)
	}

	return fmt.Sprintf("%-34s %6d requests | answered in %6.2fs, stored in %6.2fs = %6.0f/s | answer p50 %8v p99 %8v max %8v | rejected %d | memory %d MB before, %d peak, %d after",
		result.name, result.requests, result.accepted.Seconds(), result.stored.Seconds(), float64(result.requests)/result.stored.Seconds(),
		at(.5), at(.99), at(1), result.rejected, result.before, result.peak, result.after)
}

// sendBurst sends perProject envelopes from every project at once (one
// sender per project) and waits until stored reports them all.
func sendBurst(t *testing.T, name string, handler *Handler, projects config.Projects, perProject int, envelope []byte, issues int, stored func() int64) burst {
	result := burst{name: name, requests: len(projects) * perProject, before: currentRSS()}
	sampling := make(chan struct{})
	sampled := make(chan int64)

	go func() {
		peak := currentRSS()

		for {
			select {
			case <-sampling:
				sampled <- peak

				return
			case <-time.After(20 * time.Millisecond):
				peak = max(peak, currentRSS())
			}
		}
	}()

	// INGEST_HEAP=file writes a heap profile at the burst's largest live heap.
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

					if file, err := os.Create(path + "." + strings.Fields(name)[1]); err == nil {
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

	for stored() < int64(result.requests-result.rejected) {
		time.Sleep(5 * time.Millisecond)
	}

	result.stored = time.Since(start)

	close(sampling)

	result.peak = <-sampled

	debug.FreeOSMemory()

	result.after = currentRSS()

	return result
}

// The target of docs/storage-plan.md: 50 DSNs sending 1000 errors each at
// once, through SQLite as today and through the inbox and the key-value store. Run with
// INGEST_LOAD=1 go test ./internal/ingest -run TestIngestLoad -v
// (INGEST_LOAD=kv for the key-value store only). The database is in a
// temporary directory, on tmpfs on many systems, where a disk sync costs
// nothing: INGEST_DIR=<dir> puts it on the disk to measure.
func TestIngestLoad(t *testing.T) {
	if os.Getenv("INGEST_LOAD") == "" {
		t.Skip("set INGEST_LOAD=1")
	}

	// INGEST_SCALE=100 sends 100 events and 20 log envelopes per project
	// (5000 events in all); the default is the target, 1000 and 200.
	projectCount, eventsPerProject, logsPerProject := 50, 1000, 200

	if scale := os.Getenv("INGEST_SCALE"); scale != "" {
		_, _ = fmt.Sscan(scale, &eventsPerProject)
		logsPerProject = max(1, eventsPerProject/5)
	}

	projects := benchProjects(projectCount)
	settings := &config.Config{Projects: projects, AllowedOrigins: []string{"*"}}
	event, logs := loadTestData(t, "node-event-unhandled.envelope"), loadTestData(t, "node-logs.envelope")
	retention := config.Retention{EventsPerIssue: 20, LogsPerMessage: 10}

	var reports []string

	// Today: SQLite, stored within the request (INGEST_LOAD=kv skips it).
	if os.Getenv("INGEST_LOAD") != "kv" {
		database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "load.db"))

		if err != nil {
			t.Fatal(err)
		}

		var counted atomic.Int64

		sink := countingSink{Sink: StoreSink{Store: database, Logger: slog.New(slog.DiscardHandler), Retention: retention}, counted: &counted}
		handler := New(slog.New(slog.DiscardHandler), settings, projects, sink)
		reports = append(reports, sendBurst(t, "sqlite: events", handler, projects, eventsPerProject, event, 20, counted.Load).String())
		counted.Store(0)
		reports = append(reports, sendBurst(t, "sqlite: logs (11 per envelope)", handler, projects, logsPerProject, logs, 0, counted.Load).String())
		_ = database.Close()
	}

	// The plan: inbox, processors, writer, key-value store. Events are not
	// capped as they are written (the cleanup trims them); repeated log
	// messages are.
	{
		dir := t.TempDir()

		if parent := os.Getenv("INGEST_DIR"); parent != "" {
			var err error

			if dir, err = os.MkdirTemp(parent, "ingest-load-"); err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = os.RemoveAll(dir) })
		}

		database, err := kvstore.Open(dir, nil)

		if err != nil {
			t.Fatal(err)
		}

		handler := New(slog.New(slog.DiscardHandler), settings, projects, nil)
		pipeline := NewPipeline(handler, database, StoreSink{Logger: slog.New(slog.DiscardHandler), Retention: config.Retention{LogsPerMessage: logsPerMessage()}})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})

		go func() { defer close(done); pipeline.Run(ctx) }()

		// INGEST_ONLY=logs skips the events, to profile the logs alone.
		if os.Getenv("INGEST_ONLY") != "logs" {
			reports = append(reports, sendBurst(t, "kv: events", handler, projects, eventsPerProject, event, 20, pipeline.Stored).String())
		}

		before := pipeline.Stored()

		reports = append(reports, sendBurst(t, "kv: logs (11 per envelope)", handler, projects, logsPerProject, logs, 0, func() int64 { return pipeline.Stored() - before }).String())

		cancel()
		<-done

		var memory runtime.MemStats

		runtime.ReadMemStats(&memory)

		_ = database.Close()

		var size int64

		_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				size += info.Size()
			}

			return nil
		})

		reports = append(reports, fmt.Sprintf("kv: database %d MB on disk, Go heap %d MB", size>>20, memory.HeapAlloc>>20))
	}

	for _, report := range reports {
		t.Log(report)
	}
}

// countingSink counts envelopes stored by the SQLite path.
type countingSink struct {
	Sink
	counted *atomic.Int64
}

func (sink countingSink) StoreEvent(ctx context.Context, project config.Project, event *sentry.Event) error {
	defer sink.counted.Add(1)

	return sink.Sink.StoreEvent(ctx, project, event)
}

func (sink countingSink) StoreLogs(ctx context.Context, project config.Project, logs []sentry.Log) error {
	defer sink.counted.Add(1)

	return sink.Sink.StoreLogs(ctx, project, logs)
}

// logsPerMessage is the cap on repeated log messages of the load test:
// INGEST_REPEATS=0 turns it off to measure the write path alone (the test
// sends the same 11 lines again and again, so with the cap nearly every log
// is deleted as soon as it is written).
func logsPerMessage() int {
	if value := os.Getenv("INGEST_REPEATS"); value != "" {
		limit, _ := strconv.Atoi(value)

		return limit
	}

	return config.DefaultRetentionLogsPerMessage
}
