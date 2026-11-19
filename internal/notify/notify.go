// Package notify sends alerts about new and regressed issues to the
// connections set up in the UI, delivered through the outbox.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Kind is why a notification is sent.
type Kind string

const (
	// NewIssue: an error seen for the first time.
	NewIssue Kind = "new_issue"
	// Regression: a resolved issue happened again.
	Regression Kind = "regression"
	// Test: the "Send test" button of a connection.
	Test Kind = "test"
)

// Notification is one alert about an issue.
type Notification struct {
	Kind      Kind
	ProjectID int
	Project   string
	// ProjectColor marks the project in emails ("#rrggbb"; may be empty).
	ProjectColor string `json:",omitempty"`
	IssueID      string
	IssueURL     string // link to the issue in the UI; empty without PUBLIC_URL
	Title        string
	Culprit      string
	Level        string
	Environment  string
	Release      string
	Time         time.Time
	// Event is the full event, for formats with room for details (email). May
	// be nil. In the outbox it is stored as the original JSON and parsed again.
	Event *sentry.Event `json:"-"`
}

// Summary stands for notifications held back by the flood limit.
type Summary struct {
	Count  int
	Window time.Duration
	// Items are the first few held notifications, to show what they were about.
	Items []Notification
}

// maxSummaryItems is how many held notifications a summary lists.
const maxSummaryItems = 10

// Channel delivers notifications to one destination.
type Channel interface {
	Name() string
	Send(ctx context.Context, notification Notification) error
	SendSummary(ctx context.Context, summary Summary) error
}

// RetryAfterError asks the notifier to retry after Delay (a rate limit response).
type RetryAfterError struct {
	Delay time.Duration
	Err   error
}

func (err *RetryAfterError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", err.Err, err.Delay)
}

func (err *RetryAfterError) Unwrap() error { return err.Err }

// Store is what the notifier needs from the database (implemented by store.Store).
type Store interface {
	ListConnections(ctx context.Context) ([]store.Connection, error)
	GetConnection(ctx context.Context, id string) (store.Connection, error)

	EnqueueOutbox(ctx context.Context, channels []string, kind string, payload, eventPayload []byte, now time.Time) error
	DueOutbox(ctx context.Context, channel string, now time.Time, limit int) ([]store.OutboxMessage, error)
	PendingChannels(ctx context.Context) ([]string, error)
	MarkOutboxSent(ctx context.Context, id int64, now time.Time) error
	MarkOutboxRetry(ctx context.Context, id int64, lastError string, next, now time.Time) error
	MarkOutboxFailed(ctx context.Context, id int64, lastError string, now time.Time) error
	CleanOutbox(ctx context.Context, sentBefore, failedBefore time.Time) (int64, error)
}

// ChannelFactory builds the channel of a connection (implemented by Factory).
type ChannelFactory interface {
	Channel(connection store.Connection) (Channel, error)
}

// Options configures a Notifier. Zero values get the defaults below.
type Options struct {
	Store    Store
	Channels ChannelFactory
	Logger   *slog.Logger

	MaxPerWindow int           // per connection; default 5
	Window       time.Duration // default 1 minute
	QueueSize    int           // in-memory notifications not yet in the outbox; default 1000

	// Schedule is the wait before each retry; the last one repeats.
	Schedule   []time.Duration
	RetryFor   time.Duration // default 1 day
	KeepSent   time.Duration // default 1 day
	KeepFailed time.Duration // default 30 days

	PollInterval time.Duration // how often workers look for due retries; default 1s
	Now          func() time.Time
}

var defaultSchedule = []time.Duration{
	2 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute,
	5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour,
}

// Message kinds in the outbox.
const (
	kindNotification = "notification"
	kindSummary      = "summary"
)

// sendTimeout bounds one attempt to reach a chat or mail server.
const sendTimeout = 30 * time.Second

// dueBatch is how many due messages a worker reads at once.
const dueBatch = 20

type Notifier struct {
	options Options
	queue   chan Notification

	// ctx is cancelled when Close gives up waiting: sends in progress stop.
	ctx    context.Context
	cancel context.CancelFunc

	dispatched chan struct{} // closed when the dispatcher has written everything
	done       chan struct{}
	running    sync.WaitGroup

	workersMutex sync.Mutex
	workers      map[string]*worker // by outbox channel (the connection id)

	mutex  sync.Mutex
	closed bool

	// connections are read again once the queue has emptied: a burst of new
	// issues reads them once, and an edit applies to the next notification.
	connections []store.Connection
	inBurst     bool
}

type worker struct {
	wake chan struct{} // new messages in the outbox
}

// window is the flood limit state of one connection.
type window struct {
	sent int
	// held counts the notifications over the limit; examples keeps the first
	// maxSummaryItems of them for the summary. Only those are kept: a flood of
	// errors with large events must not pile up in memory until the window ends.
	held     int
	examples []Notification
}

// New starts the goroutines. Without a store Notify does nothing.
func New(options Options) *Notifier {
	setDefaults(&options)

	ctx, cancel := context.WithCancel(context.Background())
	notifier := &Notifier{
		options:    options,
		queue:      make(chan Notification, options.QueueSize),
		ctx:        ctx,
		cancel:     cancel,
		dispatched: make(chan struct{}),
		done:       make(chan struct{}),
		workers:    map[string]*worker{},
	}

	if options.Store == nil || options.Channels == nil {
		close(notifier.done)

		return notifier
	}

	// Messages left from before a restart get their workers right away.
	pending, err := options.Store.PendingChannels(ctx)

	if err != nil {
		options.Logger.Error("read pending notifications", "err", err)
	}

	notifier.running.Go(func() {
		for _, channel := range pending {
			notifier.wakeWorker(channel)
		}

		notifier.dispatch()
		close(notifier.dispatched)
	})

	go func() {
		notifier.running.Wait()
		close(notifier.done)
	}()

	return notifier
}

func setDefaults(options *Options) {
	if options.MaxPerWindow <= 0 {
		options.MaxPerWindow = 5
	}

	if options.Window <= 0 {
		options.Window = time.Minute
	}

	if options.QueueSize <= 0 {
		options.QueueSize = 1000
	}

	if len(options.Schedule) == 0 {
		options.Schedule = defaultSchedule
	}

	if options.RetryFor <= 0 {
		options.RetryFor = config.DefaultNotifyRetryFor
	}

	if options.KeepSent <= 0 {
		options.KeepSent = config.DefaultNotifyKeepSent
	}

	if options.KeepFailed <= 0 {
		options.KeepFailed = config.DefaultNotifyKeepFailed
	}

	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}

	if options.Now == nil {
		options.Now = time.Now
	}

	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
}

// Notify queues a notification. It never blocks: when the in-memory queue is
// full (the database is stuck) the notification is dropped and logged.
func (notifier *Notifier) Notify(notification Notification) {
	if notifier.options.Store == nil {
		return
	}

	notifier.mutex.Lock()
	defer notifier.mutex.Unlock()

	if notifier.closed {
		return
	}

	select {
	case notifier.queue <- notification:
	default:
		notifier.options.Logger.Warn("notification queue full, dropping", "issue_id", notification.IssueID, "title", notification.Title)
	}
}

// Close stops accepting notifications, writes the queued ones (and the
// pending summaries) to the outbox, sends what is due and returns. Messages
// waiting for a retry stay in the outbox and are sent after the next start.
// If ctx ends first, sends in progress are cancelled.
func (notifier *Notifier) Close(ctx context.Context) error {
	notifier.mutex.Lock()

	if !notifier.closed {
		notifier.closed = true

		if notifier.options.Store != nil {
			close(notifier.queue)
		}
	}

	notifier.mutex.Unlock()

	select {
	case <-notifier.done:
		return nil
	case <-ctx.Done():
		notifier.cancel()

		<-notifier.done

		return ctx.Err()
	}
}

// dispatch routes notifications to connections, applies the flood limit per
// connection and writes messages to the outbox.
func (notifier *Notifier) dispatch() {
	ticker := time.NewTicker(notifier.options.Window)

	defer ticker.Stop()

	windows := map[string]*window{}

	flush := func() {
		for connectionID, current := range windows {
			if current.held > 0 {
				summary := Summary{Count: current.held, Window: notifier.options.Window, Items: current.examples}
				notifier.enqueue([]string{connectionID}, kindSummary, summary, nil)
			}
		}

		clear(windows)
	}

	for {
		select {
		case notification, ok := <-notifier.queue:
			if !ok {
				flush()

				return
			}

			notifier.route(notification, windows)
		case <-ticker.C:
			flush()
		}
	}
}

func (notifier *Notifier) route(notification Notification, windows map[string]*window) {
	if !notifier.inBurst {
		connections, err := notifier.options.Store.ListConnections(notifier.ctx)

		if err != nil {
			notifier.options.Logger.Error("read notification connections", "err", err)

			return
		}

		notifier.connections = connections
	}

	notifier.inBurst = len(notifier.queue) > 0

	var eventPayload []byte

	if notification.Event != nil {
		eventPayload = notification.Event.Raw
	}

	var send []string

	for _, connection := range notifier.connections {
		if !matches(connection, notification) {
			continue
		}

		current := windows[connection.ID]

		if current == nil {
			current = &window{}
			windows[connection.ID] = current
		}

		if current.sent < notifier.options.MaxPerWindow {
			send = append(send, connection.ID)
			current.sent++
		} else {
			current.held++

			// The summary shows what an issue was about, never its event:
			// keeping the event (up to 20 MB) would bring the memory back.
			if len(current.examples) < maxSummaryItems {
				example := notification
				example.Event = nil
				current.examples = append(current.examples, example)
			}
		}
	}

	if len(send) > 0 {
		notifier.enqueue(send, kindNotification, notification, eventPayload)
	}
}

// enqueue writes a message for connections in one batch and wakes their workers.
func (notifier *Notifier) enqueue(connectionIDs []string, kind string, message any, eventPayload []byte) {
	payload, err := json.Marshal(message)

	if err != nil {
		notifier.options.Logger.Error("encode notification", "err", err)

		return
	}

	// The outbox channel of a connection is its id.
	if err := notifier.options.Store.EnqueueOutbox(notifier.ctx, connectionIDs, kind, payload, eventPayload, notifier.options.Now()); err != nil {
		notifier.options.Logger.Error("write notification to outbox", "connection_ids", connectionIDs, "err", err)

		return
	}

	for _, connectionID := range connectionIDs {
		notifier.wakeWorker(connectionID)
	}
}

// wakeWorker starts the worker of an outbox channel if needed and tells it
// there is work. Workers are only started from the dispatcher goroutine,
// which is itself counted in running, so the WaitGroup is never at zero here.
func (notifier *Notifier) wakeWorker(channel string) {
	notifier.workersMutex.Lock()
	defer notifier.workersMutex.Unlock()

	current := notifier.workers[channel]

	if current == nil {
		current = &worker{wake: make(chan struct{}, 1)}
		notifier.workers[channel] = current

		notifier.running.Go(func() { notifier.runWorker(channel, current) })
	}

	select {
	case current.wake <- struct{}{}:
	default: // already woken
	}
}

// runWorker sends the due messages of one connection until Close: woken by
// new messages, and every PollInterval for retries that became due.
func (notifier *Notifier) runWorker(channel string, current *worker) {
	ticker := time.NewTicker(notifier.options.PollInterval)

	defer ticker.Stop()

	for {
		select {
		case <-current.wake:
		case <-ticker.C:
		case <-notifier.dispatched:
			// Everything is in the outbox: one last pass, then stop.
			notifier.sendDue(channel)

			return
		case <-notifier.ctx.Done():
			return
		}

		notifier.sendDue(channel)
	}
}

func (notifier *Notifier) sendDue(channel string) {
	for notifier.ctx.Err() == nil {
		messages, err := notifier.options.Store.DueOutbox(notifier.ctx, channel, notifier.options.Now(), dueBatch)

		if err != nil {
			notifier.options.Logger.Error("read notification outbox", "connection", channel, "err", err)

			return
		}

		if len(messages) == 0 {
			return
		}

		// One channel per batch: the connection is read and its secret decrypted once.
		built, err := notifier.channel(channel)

		for _, message := range messages {
			notifier.attempt(message, built, err)
		}

		if len(messages) < dueBatch {
			return
		}
	}
}

// errGone are failures that retrying cannot fix.
var errGone = errors.New("gone")

// attempt sends one message once through channel (or records why there is
// none, channelErr) and records the result.
func (notifier *Notifier) attempt(message store.OutboxMessage, channel Channel, channelErr error) {
	about := []any{"connection", message.Channel, "outbox_id", message.ID, "attempt", message.Attempts + 1}
	err := channelErr

	if err == nil {
		ctx, cancel := context.WithTimeout(notifier.ctx, sendTimeout)
		err = send(ctx, channel, message)

		cancel()

		about = append(about, "channel", channel.Name())
	}

	now := notifier.options.Now()

	// The result is recorded even when a shutdown cancelled the send.
	record := context.Background()

	if err == nil {
		if err := notifier.options.Store.MarkOutboxSent(record, message.ID, now); err != nil {
			notifier.options.Logger.Error("mark notification sent", append(about, "err", err)...)
		}

		return
	}

	schedule := notifier.options.Schedule
	delay := schedule[min(message.Attempts, len(schedule)-1)]

	var retryAfter *RetryAfterError

	if errors.As(err, &retryAfter) && retryAfter.Delay > 0 {
		delay = retryAfter.Delay
	}

	next := now.Add(delay)

	if errors.Is(err, errGone) || next.Sub(message.CreatedAt) > notifier.options.RetryFor {
		notifier.options.Logger.Error("notification failed, giving up", append(about, "err", err)...)

		if err := notifier.options.Store.MarkOutboxFailed(record, message.ID, err.Error(), now); err != nil {
			notifier.options.Logger.Error("mark notification failed", append(about, "err", err)...)
		}

		return
	}

	notifier.options.Logger.Warn("notification attempt failed, retrying", append(about, "retry_in", delay, "err", err)...)

	if err := notifier.options.Store.MarkOutboxRetry(record, message.ID, err.Error(), next, now); err != nil {
		notifier.options.Logger.Error("schedule notification retry", append(about, "err", err)...)
	}
}

// channel builds the channel of an outbox channel id from the stored
// connection, so edits made in the UI apply to queued messages too.
func (notifier *Notifier) channel(outboxChannel string) (Channel, error) {
	connection, err := notifier.options.Store.GetConnection(notifier.ctx, outboxChannel)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("%w: the connection was deleted", errGone)
	case err != nil:
		return nil, err
	case !connection.Active:
		return nil, fmt.Errorf("%w: the connection is disabled", errGone)
	}

	return notifier.options.Channels.Channel(connection)
}

func send(ctx context.Context, channel Channel, message store.OutboxMessage) error {
	switch message.Kind {
	case kindSummary:
		var summary Summary

		if err := json.Unmarshal(message.Payload, &summary); err != nil {
			return fmt.Errorf("%w: unreadable message: %w", errGone, err)
		}

		return channel.SendSummary(ctx, summary)
	case kindNotification:
		var notification Notification

		if err := json.Unmarshal(message.Payload, &notification); err != nil {
			return fmt.Errorf("%w: unreadable message: %w", errGone, err)
		}

		if len(message.EventPayload) > 0 {
			// The event was valid when received, so this only fails on corrupted
			// data; the notification still goes out, without details.
			notification.Event, _ = sentry.ParseEvent(message.EventPayload, "", notification.Time)
		}

		return channel.Send(ctx, notification)
	default:
		return fmt.Errorf("%w: unknown message kind %q", errGone, message.Kind)
	}
}

// CleanOutbox deletes sent messages older than KeepSent and failed ones older than KeepFailed.
func (notifier *Notifier) CleanOutbox(ctx context.Context) error {
	now := notifier.options.Now()
	deleted, err := notifier.options.Store.CleanOutbox(ctx, now.Add(-notifier.options.KeepSent), now.Add(-notifier.options.KeepFailed))

	if err == nil && deleted > 0 {
		notifier.options.Logger.Debug("cleaned notification outbox", "deleted", deleted)
	}

	return err
}
