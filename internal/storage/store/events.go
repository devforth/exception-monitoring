package store

import (
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/text"
)

// Issue statuses.
const (
	StatusUnresolved = "unresolved"
	StatusResolved   = "resolved"
	StatusIgnored    = "ignored"
)

// NewEvent is an event ready to store: already parsed and grouped.
// The store does not know the Sentry format; the caller fills this in.
type NewEvent struct {
	ProjectID   int
	EventID     string
	Fingerprint string
	Title       string
	Culprit     string
	Level       string
	Environment string
	Release     string
	TraceID     string
	// SpanID is the span the error was captured in, ParentSpanID its parent
	// (usually the HTTP request span). Logs of the request carry one of them.
	SpanID       string
	ParentSpanID string
	Timestamp    time.Time // when it happened, by the SDK's clock
	ReceivedAt   time.Time // when the server got it
	Payload      []byte    // full event JSON
	// KeepEvents > 0 keeps only the newest KeepEvents events of the issue:
	// older ones are deleted as this one is saved (RETENTION_EVENTS_PER_ISSUE).
	KeepEvents int
}

// An issue keeps at most this much of an event's title and culprit: titles
// come from error messages and can be huge.
const (
	MaxTitleLength   = 500
	MaxCulpritLength = 300
)

// IssueText is the title and culprit an issue keeps of event.
func (event NewEvent) IssueText() (title, culprit string) {
	return text.Shorten(event.Title, MaxTitleLength), text.Shorten(event.Culprit, MaxCulpritLength)
}

// SaveResult tells what storing an event changed. Notifications (stage 6) use it.
type SaveResult struct {
	IssueID int64
	// Duplicate: an event with this event_id was already stored (an SDK retry); nothing changed.
	Duplicate bool
	// NewIssue: the first event of a new issue.
	NewIssue bool
	// Regression: the issue was resolved and happened again, so it is unresolved now.
	Regression bool
}

// NewLog is a log record ready to store.
type NewLog struct {
	ProjectID      int
	Timestamp      time.Time
	Level          string
	SeverityNumber int
	Body           string
	TraceID        string
	SpanID         string // the span the log was written in
	Attributes     []byte // JSON object
	// Fingerprint groups repeats of the same message (grouping.LogFingerprint).
	Fingerprint string
}
