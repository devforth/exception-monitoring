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
	"github.com/devforth/exception-monitoring/internal/storage/database"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Pipeline stores envelopes through the inbox: processors parse them, one writer stores them.
type Pipeline struct {
	handler *Handler
	store   *database.Store
	sink    StoreSink // maps parsed events and logs to records and reports them (its Store is unused)
	logger  *slog.Logger

	// MaxInboxBytes: beyond it new envelopes get 429.
	MaxInboxBytes int64
	// BatchSize, BatchBytes and BatchWait bound one write.
	BatchSize  int
	BatchBytes int
	BatchWait  time.Duration
	// LogsPerMessage caps the copies of a repeated log message (0: none).
	LogsPerMessage int
	// Rescan is how often the inbox is read again for entries missed.
	Rescan time.Duration

	// fresh takes envelopes straight from requests; when full, missed asks for a rescan.
	fresh  chan database.InboxEntry
	missed atomic.Bool
	wake   chan struct{}

	stored atomic.Int64 // envelopes stored since start
	// inFlight holds entries handed out and not stored yet.
	inFlight sync.Map
}

// processed is a parsed entry; events match Envelope.Events in order.
type processed struct {
	database.Envelope
	project config.Project
	events  []*sentry.Event
}

// NewPipeline makes handler write to the inbox; sink maps and reports (its Store is unused).
func NewPipeline(handler *Handler, db *database.Store, sink StoreSink) *Pipeline {
	pipeline := &Pipeline{
		handler: handler, store: db, sink: sink, logger: handler.logger,
		MaxInboxBytes: 1 << 30, LogsPerMessage: config.LogsPerMessage, BatchSize: 512, BatchBytes: 4 << 20, BatchWait: 5 * time.Millisecond, Rescan: 10 * time.Second,
		fresh: make(chan database.InboxEntry, 4*runtime.GOMAXPROCS(0)), wake: make(chan struct{}, 1),
	}
	handler.pipeline = pipeline

	return pipeline
}

func (pipeline *Pipeline) Stored() int64 { return pipeline.stored.Load() }

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

func (pipeline *Pipeline) Run(ctx context.Context) {
	workers := runtime.GOMAXPROCS(0)

	// Small queues: envelopes wait in the inbox on disk, not in memory.
	entries := make(chan database.InboxEntry, workers*2)
	results := make(chan processed, workers*4)

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

// dispatch hands fresh entries, and the inbox's at start, after a full queue and every Rescan.
func (pipeline *Pipeline) dispatch(ctx context.Context, entries chan<- database.InboxEntry) {
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

func (pipeline *Pipeline) hand(ctx context.Context, entries chan<- database.InboxEntry, entry database.InboxEntry) bool {
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

// handInbox checks each entry is still waiting, so none is stored twice.
func (pipeline *Pipeline) handInbox(ctx context.Context, entries chan<- database.InboxEntry) bool {
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

// process parses an envelope; a malformed one only leaves the inbox.
func (pipeline *Pipeline) process(entry database.InboxEntry) processed {
	result := processed{Envelope: database.Envelope{Sequence: entry.Sequence, InboxSize: entry.Size}}
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

	result.LogsPerMessage = pipeline.LogsPerMessage

	return result
}

// write commits results in batches and reports the stored events.
func (pipeline *Pipeline) write(ctx context.Context, results <-chan processed) {
	for first := range results {
		batch := []processed{first}
		size := first.InboxSize
		timer := time.NewTimer(pipeline.BatchWait)

	collect:
		for len(batch) < pipeline.BatchSize && size < pipeline.BatchBytes {
			select {
			case next, ok := <-results:
				if !ok {
					break collect
				}

				batch = append(batch, next)
				size += next.InboxSize
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

// storeBatch retries every second; on shutdown the batch stays in the inbox.
func (pipeline *Pipeline) storeBatch(ctx context.Context, batch []processed) ([][]store.SaveResult, bool) {
	envelopes := make([]database.Envelope, len(batch))

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
