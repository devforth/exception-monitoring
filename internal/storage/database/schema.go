package database

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// The records of the store and their collections: the one place where keys,
// indexes and encodings are declared (docs/storage-plan.md, "Data layout").
// Nothing else builds a key.

// collections are the store's collections, defined on one schema.
type collections struct {
	inbox  *kv.Collection[InboxEntry]
	issues *kv.Collection[issueRecord]
	status *kv.Collection[statusRecord]
	events *kv.Collection[eventRecord]
	logs   *kv.Collection[logRecord]
	marks  *kv.Collection[timeMark]
}

// Kinds of the byte counters: what records of a kind, level and day take.
const (
	kindLog   byte = 'l'
	kindEvent byte = 'e'
	kindIssue byte = 'i'
)

// sizeCounter is the counter of a kind, level rank and day.
func sizeCounter(kind, rank byte, day int32) kv.Key {
	return kv.K(kv.U8(kind), kv.U8(rank), kv.U32(uint32(day)))
}

func defineCollections(schema *kv.Schema) collections {
	return collections{
		// inbox/<seq>: an envelope waiting to be stored.
		inbox: kv.Define(schema, kv.Definition[InboxEntry]{
			Name:   "inbox",
			Key:    func(entry InboxEntry) kv.Key { return kv.K(kv.U64(entry.Sequence)) },
			Encode: encodeInbox,
			Decode: decodeInbox,
		}),
		// issue/<id>; the whole table is also kept in memory (issues.go).
		issues: kv.Define(schema, kv.Definition[issueRecord]{
			Name:    "issue",
			Key:     func(issue issueRecord) kv.Key { return kv.K(kv.U64(uint64(issue.ID))) },
			Counter: func(issueRecord) kv.Key { return sizeCounter(kindIssue, 0, 0) },
		}),
		// status/<issue>/<seq>: the status changes of an issue, oldest first.
		status: kv.Define(schema, kv.Definition[statusRecord]{
			Name: "status",
			Key: func(change statusRecord) kv.Key {
				return kv.K(kv.U64(uint64(change.IssueID)), kv.U64(change.Sequence))
			},
		}),
		// event/<issue>/<seq>: the events of an issue, oldest first.
		events: kv.Define(schema, kv.Definition[eventRecord]{
			Name: "event",
			Key: func(event eventRecord) kv.Key {
				return kv.K(kv.U64(uint64(event.IssueID)), kv.U64(event.Sequence))
			},
			Indexes: []kv.Index[eventRecord]{
				// The SDK's event_id per project: a retry is recognized.
				{Name: "id", Unique: true, Key: func(event eventRecord) kv.Key { return eventIDKey(event.ProjectID, event.EventID) }},
				// Every event by level, then arrival: lists of all events, cleanup.
				{Name: "age", Key: func(event eventRecord) kv.Key {
					return kv.K(kv.U8(levelRank(event.Level)), kv.U64(event.Sequence))
				}},
				{Name: "trace", Key: func(event eventRecord) kv.Key {
					return kv.KIf(event.TraceID != "", kv.Text(event.TraceID), kv.U64(event.Sequence))
				}},
				// The logs of a request find its error by span or parent span.
				{Name: "span", Key: func(event eventRecord) kv.Key {
					return kv.KIf(event.SpanID != "", kv.Text(event.SpanID), kv.U64(event.Sequence))
				}},
				{Name: "parent", Key: func(event eventRecord) kv.Key {
					return kv.KIf(event.ParentSpanID != "", kv.Text(event.ParentSpanID), kv.U64(event.Sequence))
				}},
			},
			Encode: encodeEvent,
			Decode: decodeEvent,
			Counter: func(event eventRecord) kv.Key {
				return sizeCounter(kindEvent, levelRank(event.Level), dayOf(event.ReceivedAt))
			},
		}),
		// log/<level>/<seq>: logs by level, then arrival.
		logs: kv.Define(schema, kv.Definition[logRecord]{
			Name: "log",
			Key:  func(record logRecord) kv.Key { return kv.K(kv.U8(levelRank(record.Level)), kv.U64(record.Sequence)) },
			Indexes: []kv.Index[logRecord]{
				{Name: "project", Key: func(record logRecord) kv.Key {
					return kv.K(kv.U32(uint32(record.ProjectID)), kv.U8(levelRank(record.Level)), kv.U64(record.Sequence))
				}},
				{Name: "trace", Key: func(record logRecord) kv.Key {
					return kv.KIf(record.TraceID != "", kv.Text(record.TraceID), kv.U64(record.Sequence))
				}},
				{Name: "span", Key: func(record logRecord) kv.Key {
					return kv.KIf(record.SpanID != "", kv.Text(record.SpanID), kv.U64(record.Sequence))
				}},
				// The copies of a message logged over and over, oldest first.
				{Name: "message", Key: func(record logRecord) kv.Key {
					return kv.KIf(record.Fingerprint != "", append(messageParts(record.ProjectID, record.Fingerprint), kv.U64(record.Sequence))...)
				}},
			},
			Encode: encodeLog,
			Decode: decodeLog,
			Counter: func(record logRecord) kv.Key {
				return sizeCounter(kindLog, levelRank(record.Level), record.ReceivedDay)
			},
		}),
		// mark/<interval>: the first sequence number of a 10-minute interval.
		marks: kv.Define(schema, kv.Definition[timeMark]{
			Name: "mark",
			Key:  func(mark timeMark) kv.Key { return kv.K(kv.U64(mark.Interval)) },
		}),
	}
}

// eventIDKey and messageParts are the leading parts of the "id" and
// "message" index keys, shared by the declarations and the lookups.
func eventIDKey(projectID int, eventID string) kv.Key {
	return kv.K(kv.U32(uint32(projectID)), kv.Text(eventID))
}

func messageParts(projectID int, fingerprint string) []kv.Part {
	return []kv.Part{kv.U32(uint32(projectID)), kv.Text(fingerprint)}
}

// InboxEntry is an envelope waiting in the inbox.
type InboxEntry struct {
	Sequence  uint64
	ProjectID int
	Received  time.Time
	Body      []byte
	Size      int // bytes the entry takes in the inbox (InboxBytes)
}

// An inbox entry is its sequence number, project and receive time, then
// the raw body.
const inboxHeader = 8 + 4 + 8

func encodeInbox(entry InboxEntry) ([]byte, error) {
	value := make([]byte, inboxHeader, inboxHeader+len(entry.Body))
	binary.BigEndian.PutUint64(value, entry.Sequence)
	binary.BigEndian.PutUint32(value[8:], uint32(entry.ProjectID))
	binary.BigEndian.PutUint64(value[12:], uint64(entry.Received.UnixNano()))

	return append(value, entry.Body...), nil
}

var errShortRecord = errors.New("database: record too short")

func decodeInbox(value []byte) (InboxEntry, error) {
	if len(value) < inboxHeader {
		return InboxEntry{}, errShortRecord
	}

	return InboxEntry{
		Sequence:  binary.BigEndian.Uint64(value),
		ProjectID: int(binary.BigEndian.Uint32(value[8:])),
		Received:  time.Unix(0, int64(binary.BigEndian.Uint64(value[12:]))).UTC(),
		Body:      bytes.Clone(value[inboxHeader:]),
		Size:      len(value),
	}, nil
}

// statusRecord is a status change of an issue.
type statusRecord struct {
	IssueID  int64  `json:"issue_id"`
	Sequence uint64 `json:"seq"`
	model.StatusChange
}

// eventRecord is a stored event: its metadata, then its payload as sent.
type eventRecord struct {
	eventMeta
	Payload []byte
}

type eventMeta struct {
	Sequence     uint64    `json:"seq"`
	ProjectID    int       `json:"project_id"`
	IssueID      int64     `json:"issue_id"`
	EventID      string    `json:"event_id"`
	Level        string    `json:"level"`
	Environment  string    `json:"environment,omitempty"`
	Release      string    `json:"release,omitempty"`
	TraceID      string    `json:"trace_id,omitempty"`
	SpanID       string    `json:"span_id,omitempty"`
	ParentSpanID string    `json:"parent_span_id,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	ReceivedAt   time.Time `json:"received_at"`
}

func encodeEvent(event eventRecord) ([]byte, error) { return withTail(event.eventMeta, event.Payload) }

func decodeEvent(value []byte) (eventRecord, error) {
	var event eventRecord

	tail, err := splitTail(value, &event.eventMeta)
	event.Payload = tail

	return event, err
}

// logRecord is a stored log: its fields, then its attributes as JSON.
type logRecord struct {
	logMeta
	Attributes []byte
}

type logMeta struct {
	Sequence       uint64    `json:"seq"`
	ReceivedDay    int32     `json:"day"` // for the byte counters
	ProjectID      int       `json:"project_id"`
	Timestamp      time.Time `json:"timestamp"`
	Level          string    `json:"level"`
	SeverityNumber int       `json:"severity_number,omitempty"`
	Body           string    `json:"body"`
	TraceID        string    `json:"trace_id,omitempty"`
	SpanID         string    `json:"span_id,omitempty"`
	Fingerprint    string    `json:"fingerprint,omitempty"`
}

func encodeLog(record logRecord) ([]byte, error) { return withTail(record.logMeta, record.Attributes) }

func decodeLog(value []byte) (logRecord, error) {
	var record logRecord

	tail, err := splitTail(value, &record.logMeta)
	record.Attributes = tail

	return record, err
}

// timeMark ties a 10-minute interval to the first sequence number written
// in it (marks.go).
type timeMark struct {
	Interval uint64 `json:"interval"`
	First    uint64 `json:"first"`
}

// withTail is a record value: the length of its JSON head, the head, then
// raw bytes (a payload, attributes) that need no second encoding.
func withTail(head any, tail []byte) ([]byte, error) {
	encoded, err := json.Marshal(head)

	if err != nil {
		return nil, err
	}

	value := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(encoded)+len(tail)), uint32(len(encoded)))

	return append(append(value, encoded...), tail...), nil
}

// splitTail decodes the head of a value into head and returns a copy of the
// tail.
func splitTail(value []byte, head any) ([]byte, error) {
	if len(value) < 4 || int(binary.BigEndian.Uint32(value)) > len(value)-4 {
		return nil, errShortRecord
	}

	size := 4 + int(binary.BigEndian.Uint32(value))

	if err := json.Unmarshal(value[4:size], head); err != nil {
		return nil, err
	}

	return bytes.Clone(value[size:]), nil
}
