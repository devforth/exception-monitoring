package store

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// ErrNotFound is returned when the requested record does not exist.
var ErrNotFound = errors.New("not found")

type Issue struct {
	ID         string     `json:"id"`
	ProjectID  int        `json:"project_id"`
	Title      string     `json:"title"`
	Culprit    string     `json:"culprit"`
	Level      string     `json:"level"`
	Status     string     `json:"status"`
	EventCount int        `json:"event_count"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   time.Time  `json:"last_seen"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

// EventSummary is an event without its payload, for lists.
type EventSummary struct {
	ID           int64     `json:"-"`
	ProjectID    int       `json:"project_id"`
	IssueID      string    `json:"issue_id"`
	EventID      string    `json:"event_id"`
	Timestamp    time.Time `json:"timestamp"`
	ReceivedAt   time.Time `json:"received_at"`
	Level        string    `json:"level"`
	Environment  string    `json:"environment"`
	Release      string    `json:"release"`
	TraceID      string    `json:"trace_id"`
	SpanID       string    `json:"span_id"`
	ParentSpanID string    `json:"parent_span_id"`
	// Thinned: the cleanup kept only the exception, message and the basics of
	// this old event to save space.
	Thinned bool `json:"thinned,omitempty"`
}

// Event is a stored event with the full JSON the SDK sent.
type Event struct {
	EventSummary
	Payload json.RawMessage `json:"payload"`
}

type LogRecord struct {
	ID             int64           `json:"id"`
	ProjectID      int             `json:"project_id"`
	Timestamp      time.Time       `json:"timestamp"`
	Level          string          `json:"level"`
	SeverityNumber int             `json:"severity_number"`
	Body           string          `json:"body"`
	TraceID        string          `json:"trace_id"`
	SpanID         string          `json:"span_id"`
	Attributes     json.RawMessage `json:"attributes"`
}

// IssueFilter selects issues. Zero values mean "any".
type IssueFilter struct {
	ProjectIDs []int    // any of these projects (the UI filter takes several)
	Levels     []string // any of these levels
	Status     string
	Query      string // substring of the title or culprit
	Sort       string // one of IssueSorts; "" is last_seen
	// SeenSince and SeenUntil bound the date given by DateField to
	// [SeenSince, SeenUntil). Zero means no bound.
	SeenSince time.Time
	SeenUntil time.Time
	DateField string // DateAnyEvent (default), DateFirstSeen or DateLastSeen
	Limit     int
	Offset    int
}

// Which date of an issue a date range applies to.
const (
	// DateAnyEvent: the issue had at least one event in the range.
	DateAnyEvent = "event"
	// DateFirstSeen: the issue appeared in the range (new problems).
	DateFirstSeen = "first_seen"
	// DateLastSeen: the latest event of the issue is in the range.
	DateLastSeen = "last_seen"
)

// ValidDateField reports whether field is a supported IssueFilter.DateField value.
func ValidDateField(field string) bool {
	return field == "" || field == DateAnyEvent || field == DateFirstSeen || field == DateLastSeen
}

// IssueSorts are the IssueFilter.Sort values: a field, newest or most first,
// or with _asc the reverse (the project name A to Z, _desc the reverse).
var IssueSorts = []string{
	"project", "project_desc", "last_seen", "last_seen_asc",
	"first_seen", "first_seen_asc", "event_count", "event_count_asc",
}

// ValidIssueSort reports whether sort is a supported IssueFilter.Sort value.
func ValidIssueSort(sort string) bool {
	return sort == "" || slices.Contains(IssueSorts, sort)
}

// EventFilter selects events. Events are listed newest first; BeforeID
// continues a previous page (keyset pagination, stable while events arrive).
type EventFilter struct {
	IssueID  string
	TraceID  string
	BeforeID int64
	Limit    int
}

// LogFilter selects log records, newest first, with keyset pagination like events.
type LogFilter struct {
	ProjectIDs []int    // any of these projects
	Levels     []string // any of these levels
	Query      string   // substring of the body
	TraceID    string
	SpanIDs    []string // any of these spans (used with TraceID for one request)
	Since      time.Time
	Until      time.Time
	BeforeID   int64
	Limit      int
	Offset     int // page offset for the UI; the API uses BeforeID
}

// LogCountLimit is where counting logs stops: an exact count of millions of
// matches reads them all, while a pager only needs to know there are many.
var LogCountLimit = 10000

// LogIssue is the issue of the error raised in the same request as a log line.
type LogIssue struct {
	IssueID string
	Title   string
}

// Page trims rows fetched with one extra (Limit = limit+1) back to limit and
// reports whether there are more: the extra row tells that a next page exists.
func Page[T any](rows []T, limit int) (page []T, more bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}

	return rows, false
}
