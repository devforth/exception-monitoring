package ingest

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/kvstore"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Pipeline stores envelopes through an inbox (see docs/storage-plan.md):
// the HTTP handler appends an envelope to the inbox and answers; processors,
// one per core, parse and scrub envelopes in parallel; one writer stores
// their results in batches, each together with the deletion of its inbox
// entries, so a crash neither loses nor repeats an envelope, and then
// notifies of new and regressed issues.
type Pipeline struct {
	handler *Handler
	store   *kvstore.Store
	sink    StoreSink // maps parsed events and logs to records and reports them (its Store is unused)
	logger  *slog.Logger

	// MaxInboxBytes: beyond it new envelopes get 429 until the writer catches up.
	MaxInboxBytes int64
	// Workers parse envelopes; 0 means one per core.
	Workers int
	// BatchSize and BatchWait bound one write: the writer commits after
	// BatchSize envelopes, or once no more arrive within BatchWait.
	BatchSize int
	BatchWait time.Duration
	// Rescan is how often the inbox is read again for entries that did not
	// reach the processors directly (a full queue).
	Rescan time.Duration

	// fresh hands accepted envelopes to the dispatcher straight from the
	// request; when it is full, missed asks for a rescan of the inbox.
	fresh  chan kvstore.InboxEntry
	missed atomic.Bool
	wake   chan struct{}

	stored atomic.Int64 // envelopes stored since start
	// inFlight holds the sequence numbers handed to processors and not yet
	// stored, so an entry is never handed out twice.
	inFlight sync.Map
}

// processed is an inbox entry after parsing: what to store, and the parsed
// events (in the order of Envelope.Events) to report once stored.
type processed struct {
	kvstore.Envelope
	project config.Project
	events  []*sentry.Event
}

// NewPipeline makes handler take envelopes into the inbox of database;
// sink maps and reports what is stored (its Store is not used).
func NewPipeline(handler *Handler, database *kvstore.Store, sink StoreSink) *Pipeline {
	pipeline := &Pipeline{
		handler: handler, store: database, sink: sink, logger: handler.logger,
		MaxInboxBytes: 1 << 30, BatchSize: 512, BatchWait: 5 * time.Millisecond, Rescan: 10 * time.Second,
		fresh: make(chan kvstore.InboxEntry, 4*runtime.GOMAXPROCS(0)), wake: make(chan struct{}, 1),
	}
	handler.pipeline = pipeline

	return pipeline
}

// Stored is how many envelopes were stored since the start.
func (pipeline *Pipeline) Stored() int64 { return pipeline.stored.Load() }

// accept appends an authenticated envelope to the inbox and hands it on.
func (pipeline *Pipeline) accept(project config.Project, envelope *sentry.Envelope, body []byte) (map[string]string, error) {
	if pipeline.store.InboxBytes() > pipeline.MaxInboxBytes {
		return nil, &requestError{status: http.StatusTooManyRequests, message: "server busy", retryAfter: "10"}
	}

	entry, err := pipeline.store.Append(project.ID, pipeline.handler.now(), body)

	if err != nil {
		return nil, err
	}

	select {
	case pipeline.fresh <- entry:
	default:
		// The processors are behind: the entry waits in the inbox.
		pipeline.missed.Store(true)

		select {
		case pipeline.wake <- struct{}{}:
		default:
		}
	}

	response := map[string]string{}

	if envelope.Header.EventID != "" {
		response["id"] = envelope.Header.EventID
	}

	return response, nil
}

// Run processes the inbox until ctx ends.
func (pipeline *Pipeline) Run(ctx context.Context) {
	workers := pipeline.Workers

	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	// Small queues between the stages: envelopes wait in the inbox (on disk),
	// not in memory.
	entries := make(chan kvstore.InboxEntry, workers*2)
	results := make(chan processed, pipeline.BatchSize)

	var processors sync.WaitGroup

	for range workers {
		processors.Add(1)

		go func() {
			defer processors.Done()

			for entry := range entries {
				results <- pipeline.process(entry)
			}
		}()
	}

	writerDone := make(chan struct{})

	go func() {
		defer close(writerDone)

		pipeline.write(ctx, results)
	}()

	pipeline.dispatch(ctx, entries)
	close(entries)
	processors.Wait()
	close(results)
	<-writerDone
}

// dispatch hands entries to the processors: those accepted while it runs,
// as they come, and the inbox's at the start (left by a stop or a crash),
// after a full queue and every Rescan, read again from the disk.
func (pipeline *Pipeline) dispatch(ctx context.Context, entries chan<- kvstore.InboxEntry) {
	rescan := time.NewTicker(pipeline.Rescan)

	defer rescan.Stop()

	scan := true

	for ctx.Err() == nil {
		if scan || pipeline.missed.Swap(false) {
			scan = false

			if !pipeline.handInbox(ctx, entries) {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case entry := <-pipeline.fresh:
			if !pipeline.hand(ctx, entries, entry) {
				return
			}
		case <-pipeline.wake:
		case <-rescan.C:
			scan = true
		}
	}
}

// hand gives an entry to the processors unless it is already with them;
// false when ctx ended.
func (pipeline *Pipeline) hand(ctx context.Context, entries chan<- kvstore.InboxEntry, entry kvstore.InboxEntry) bool {
	if _, busy := pipeline.inFlight.LoadOrStore(entry.Sequence, true); busy {
		return true
	}

	select {
	case entries <- entry:
		return true
	case <-ctx.Done():
		return false
	}
}

// handInbox hands every entry of the inbox not with the processors yet. An
// entry read just before the writer stored it would be stored twice, so each
// is checked to be still waiting; false when ctx ended.
func (pipeline *Pipeline) handInbox(ctx context.Context, entries chan<- kvstore.InboxEntry) bool {
	inFlight := func(sequence uint64) bool {
		_, busy := pipeline.inFlight.Load(sequence)

		return busy
	}

	for after := uint64(0); ctx.Err() == nil; {
		pending, err := pipeline.store.Pending(after, pipeline.BatchSize, inFlight)

		if err != nil {
			pipeline.logger.Error("read inbox", "err", err)

			return true
		}

		for _, entry := range pending {
			after = entry.Sequence

			if waiting, err := pipeline.store.InInbox(entry.Sequence); err != nil || !waiting {
				continue
			}

			if !pipeline.hand(ctx, entries, entry) {
				return false
			}
		}

		if len(pending) < pipeline.BatchSize {
			return true
		}
	}

	return false
}

// process parses one envelope; a malformed one stores nothing and leaves the inbox.
func (pipeline *Pipeline) process(entry kvstore.InboxEntry) processed {
	result := processed{Envelope: kvstore.Envelope{Sequence: entry.Sequence, InboxSize: entry.Size}}
	project, ok := pipeline.handler.projects.List().ByID(entry.ProjectID)

	if !ok {
		pipeline.logger.Debug("envelope of a deleted project dropped", "project_id", entry.ProjectID)

		return result
	}

	result.project = project
	envelope, err := sentry.ParseEnvelope(entry.Body)

	if err != nil {
		pipeline.logger.Debug("malformed envelope dropped", "project", project.Name, "err", err)

		return result
	}

	events, logs, err := pipeline.handler.parseItems(project, envelope, entry.Received)

	if err != nil {
		pipeline.logger.Debug("malformed envelope dropped", "project", project.Name, "err", err)

		return result
	}

	for _, event := range events {
		if record, ok := pipeline.sink.eventRecord(project, event); ok {
			record.ReceivedAt = entry.Received
			result.Events = append(result.Events, record)
			result.events = append(result.events, event)
		}
	}

	if result.Logs, err = pipeline.sink.logRecords(project, logs); err != nil {
		pipeline.logger.Debug("logs dropped", "project", project.Name, "err", err)
	}

	result.LogsPerMessage = pipeline.sink.Retention.For(project.Retention).LogsPerMessage

	return result
}

// write commits results in batches until results is closed, then reports
// each stored event. A failed batch is retried until the server stops: its
// entries are still in the inbox, so the next start stores them.
func (pipeline *Pipeline) write(ctx context.Context, results <-chan processed) {
	for first := range results {
		batch := []processed{first}
		timer := time.NewTimer(pipeline.BatchWait)

	collect:
		for len(batch) < pipeline.BatchSize {
			select {
			case next, ok := <-results:
				if !ok {
					break collect
				}

				batch = append(batch, next)
			case <-timer.C:
				break collect
			}
		}

		timer.Stop()

		saved, stored := pipeline.storeBatch(ctx, batch)

		for _, envelope := range batch {
			pipeline.inFlight.Delete(envelope.Sequence)
		}

		if !stored {
			continue
		}

		pipeline.stored.Add(int64(len(batch)))

		for index, envelope := range batch {
			for position, result := range saved[index] {
				pipeline.sink.report(envelope.project, envelope.events[position], result, envelope.Events[position].ReceivedAt)
			}
		}
	}
}

// storeBatch writes a batch, retrying every second until it is stored or
// the server stops (then the batch stays in the inbox for the next start).
func (pipeline *Pipeline) storeBatch(ctx context.Context, batch []processed) ([][]store.SaveResult, bool) {
	envelopes := make([]kvstore.Envelope, len(batch))

	for index, entry := range batch {
		envelopes[index] = entry.Envelope
	}

	for {
		saved, err := pipeline.store.Write(envelopes)

		if err == nil {
			return saved, true
		}

		pipeline.logger.Error("store batch, will retry", "envelopes", len(batch), "err", err)

		select {
		case <-ctx.Done():
			pipeline.logger.Warn("batch left in the inbox for the next start", "envelopes", len(batch))

			return nil, false
		case <-time.After(time.Second):
		}
	}
}
