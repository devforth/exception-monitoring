package database

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// The records and collections of the store: the one place where keys are declared.

type collections struct {
	inbox    *kv.Collection[InboxEntry]
	issues   *kv.Collection[issueRecord]
	status   *kv.Collection[statusRecord]
	events   *kv.Collection[eventRecord]
	logs     *kv.Collection[logRecord]
	marks    *kv.Collection[timeMark]
	thin     *kv.Collection[thinProgress]
	cleanups *kv.Collection[CleanupRun]

	users        *kv.Collection[userRecord]
	sessions     *kv.Collection[sessionRecord]
	oauthClients *kv.Collection[model.OAuthClient]
	oauthCodes   *kv.Collection[codeRecord]
	grants       *kv.Collection[grantRecord]
	spentTokens  *kv.Collection[spentToken]

	projects     *kv.Collection[model.Project]
	destinations *kv.Collection[destinationRecord]
	connections  *kv.Collection[connectionRecord]
	outbox       *kv.Collection[outboxRecord]
}

// Kinds of the byte counters.
const (
	kindLog   byte = 'l'
	kindEvent byte = 'e'
	kindIssue byte = 'i'
)

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
		// issue/<id>; also kept in memory.
		issues: kv.Define(schema, kv.Definition[issueRecord]{
			Name:    "issue",
			Key:     func(issue issueRecord) kv.Key { return kv.K(kv.Text(issue.ID)) },
			Counter: func(issueRecord) kv.Key { return sizeCounter(kindIssue, 0, 0) },
		}),
		// status/<issue>/<seq>: the status changes of an issue, oldest first.
		status: kv.Define(schema, kv.Definition[statusRecord]{
			Name: "status",
			Key: func(change statusRecord) kv.Key {
				return kv.K(kv.Text(change.IssueID), kv.U64(change.Sequence))
			},
		}),
		// event/<issue>/<seq>: the events of an issue, oldest first.
		events: kv.Define(schema, kv.Definition[eventRecord]{
			Name: "event",
			Key: func(event eventRecord) kv.Key {
				return kv.K(kv.Text(event.IssueID), kv.U64(event.Sequence))
			},
			Indexes: []kv.Index[eventRecord]{
				// The SDK's event_id per project, so a retry is recognized.
				{Name: "id", Unique: true, Key: func(event eventRecord) kv.Key { return eventIDKey(event.ProjectID, event.EventID) }},
				// By level, then arrival.
				{Name: "age", Key: func(event eventRecord) kv.Key {
					return kv.K(kv.U8(levelRank(event.Level)), kv.U64(event.Sequence))
				}},
				{Name: "trace", Key: func(event eventRecord) kv.Key {
					return kv.KIf(event.TraceID != "", kv.Text(event.TraceID), kv.U64(event.Sequence))
				}},
				{Name: "span", Key: func(event eventRecord) kv.Key {
					return kv.KIf(event.SpanID != "", kv.Text(event.SpanID), kv.U64(event.Sequence))
				}},
				{Name: "parent", Key: func(event eventRecord) kv.Key {
					return kv.KIf(event.ParentSpanID != "", kv.Text(event.ParentSpanID), kv.U64(event.Sequence))
				}},
			},
			Encode:     encodeEvent,
			Decode:     decodeEvent,
			DecodeHead: decodeEventHead,
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
				// The copies of a repeated message, oldest first.
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
		// mark/<interval>: the first sequence number of an interval.
		marks: kv.Define(schema, kv.Definition[timeMark]{
			Name: "mark",
			Key:  func(mark timeMark) kv.Key { return kv.K(kv.U64(mark.Interval)) },
		}),
		// cleanup/<seq>: the latest runs of the cleanup (cleanup.go).
		cleanups: kv.Define(schema, kv.Definition[CleanupRun]{
			Name: "cleanup",
			Key:  func(run CleanupRun) kv.Key { return kv.K(kv.U64(run.Sequence)) },
		}),
		// thin/<level>: how far old events are thinned.
		thin: kv.Define(schema, kv.Definition[thinProgress]{
			Name: "thin",
			Key:  func(progress thinProgress) kv.Key { return kv.K(kv.U8(progress.Rank)) },
		}),
		// user/<id>: a user of the panel with the password hash.
		users: kv.Define(schema, kv.Definition[userRecord]{
			Name: "user",
			Key:  func(user userRecord) kv.Key { return kv.K(kv.Text(user.ID)) },
			Indexes: []kv.Index[userRecord]{
				// Usernames are unique in any case.
				{Name: "name", Unique: true, Key: func(user userRecord) kv.Key { return kv.K(kv.Text(strings.ToLower(user.Username))) }},
				// There is one superadmin.
				{Name: "superadmin", Unique: true, Key: func(user userRecord) kv.Key {
					return kv.KIf(user.Role == model.RoleSuperadmin, kv.U8(0))
				}},
			},
		}),
		// session/<hash of the cookie>: a login to the UI.
		sessions: kv.Define(schema, kv.Definition[sessionRecord]{
			Name: "session",
			Key:  func(session sessionRecord) kv.Key { return kv.K(kv.Text(session.TokenHash)) },
			Indexes: []kv.Index[sessionRecord]{
				{Name: "user", Key: func(session sessionRecord) kv.Key {
					return kv.K(kv.Text(session.UserID), kv.Text(session.TokenHash))
				}},
			},
		}),
		// oauth_client/<client id>: an agent registered for MCP.
		oauthClients: kv.Define(schema, kv.Definition[model.OAuthClient]{
			Name: "oauth_client",
			Key:  func(client model.OAuthClient) kv.Key { return kv.K(kv.Text(client.ID)) },
		}),
		// oauth_code/<hash>: an authorization code, used once.
		oauthCodes: kv.Define(schema, kv.Definition[codeRecord]{
			Name: "oauth_code",
			Key:  func(code codeRecord) kv.Key { return kv.K(kv.Text(code.Hash)) },
			Indexes: []kv.Index[codeRecord]{
				{Name: "client", Key: func(code codeRecord) kv.Key { return kv.K(kv.Text(code.ClientID), kv.Text(code.Hash)) }},
				{Name: "user", Key: func(code codeRecord) kv.Key { return kv.K(kv.Text(code.UserID), kv.Text(code.Hash)) }},
			},
		}),
		// grant/<id>: a connected agent or a personal token, with its token hashes.
		grants: kv.Define(schema, kv.Definition[grantRecord]{
			Name: "grant",
			Key:  func(grant grantRecord) kv.Key { return kv.K(kv.Text(grant.ID)) },
			Indexes: []kv.Index[grantRecord]{
				{Name: "access", Unique: true, Key: func(grant grantRecord) kv.Key { return kv.K(kv.Text(grant.AccessHash)) }},
				{Name: "refresh", Unique: true, Key: func(grant grantRecord) kv.Key {
					return kv.KIf(grant.RefreshHash != "", kv.Text(grant.ClientID), kv.Text(grant.RefreshHash))
				}},
				{Name: "user", Key: func(grant grantRecord) kv.Key { return kv.K(kv.Text(grant.UserID), kv.Text(grant.ID)) }},
				{Name: "client", Key: func(grant grantRecord) kv.Key {
					return kv.KIf(grant.ClientID != "", kv.Text(grant.ClientID), kv.Text(grant.ID))
				}},
				// One personal token per user.
				{Name: "token", Unique: true, Key: func(grant grantRecord) kv.Key {
					return kv.KIf(grant.Kind == model.GrantToken, kv.Text(grant.UserID))
				}},
			},
		}),
		// oauth_spent/<hash>: a refresh token already used; presented again, it revokes its grant.
		spentTokens: kv.Define(schema, kv.Definition[spentToken]{
			Name: "oauth_spent",
			Key:  func(spent spentToken) kv.Key { return kv.K(kv.Text(spent.Hash)) },
		}),
		// project/<id>: a project; its id is part of the DSN.
		projects: kv.Define(schema, kv.Definition[model.Project]{
			Name: "project",
			Key:  func(project model.Project) kv.Key { return kv.K(kv.U32(uint32(project.ID))) },
			Indexes: []kv.Index[model.Project]{
				{Name: "name", Unique: true, Key: func(project model.Project) kv.Key { return kv.K(kv.Text(project.Name)) }},
				{Name: "key", Unique: true, Key: func(project model.Project) kv.Key { return kv.K(kv.Text(project.PublicKey)) }},
			},
		}),
		// destination/<id>: where notifications go, an email address or a chat.
		destinations: kv.Define(schema, kv.Definition[destinationRecord]{
			Name: "destination",
			Key:  func(destination destinationRecord) kv.Key { return kv.K(kv.Text(destination.ID)) },
			Indexes: []kv.Index[destinationRecord]{
				// The destinations of an address, a confirmed one first.
				{Name: "email", Key: func(destination destinationRecord) kv.Key {
					confirmed := byte(1)

					if destination.Verified {
						confirmed = 0
					}

					return kv.KIf(destination.Type == "email", kv.Text(destination.Address), kv.U8(confirmed), kv.Text(destination.ID))
				}},
			},
		}),
		// connection/<id>: a notification rule.
		connections: kv.Define(schema, kv.Definition[connectionRecord]{
			Name: "connection",
			Key:  func(connection connectionRecord) kv.Key { return kv.K(kv.Text(connection.ID)) },
		}),
		// outbox/<seq>: a notification message for one connection.
		outbox: kv.Define(schema, kv.Definition[outboxRecord]{
			Name: "outbox",
			Key:  func(message outboxRecord) kv.Key { return kv.K(kv.U64(uint64(message.ID))) },
			Indexes: []kv.Index[outboxRecord]{
				{Name: "channel", Key: func(message outboxRecord) kv.Key { return kv.K(kv.Text(message.Channel), kv.U64(uint64(message.ID))) }},
				// The messages still to send, per channel, oldest first.
				{Name: "pending", Key: func(message outboxRecord) kv.Key {
					return kv.KIf(message.Status == model.OutboxPending, kv.Text(message.Channel), kv.U64(uint64(message.ID)))
				}},
				{Name: "status", Key: func(message outboxRecord) kv.Key { return kv.K(kv.Text(message.Status), kv.U64(uint64(message.ID))) }},
			},
			Encode: encodeOutbox,
			Decode: decodeOutbox,
		}),
	}
}

// eventIDKey and messageParts lead the "id" and "message" index keys.
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

// An inbox entry is its sequence, project and receive time, then the body.
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
	IssueID  string `json:"issue_id"`
	Sequence uint64 `json:"seq"`
	model.StatusChange
}

// eventRecord is the event metadata, then its payload as sent.
type eventRecord struct {
	eventMeta
	Payload []byte
}

type eventMeta struct {
	Sequence     uint64    `json:"seq"`
	ProjectID    int       `json:"project_id"`
	IssueID      string    `json:"issue_id"`
	EventID      string    `json:"event_id"`
	Level        string    `json:"level"`
	Environment  string    `json:"environment,omitempty"`
	Release      string    `json:"release,omitempty"`
	TraceID      string    `json:"trace_id,omitempty"`
	SpanID       string    `json:"span_id,omitempty"`
	ParentSpanID string    `json:"parent_span_id,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	ReceivedAt   time.Time `json:"received_at"`
	Thinned      bool      `json:"thinned,omitempty"`
}

func encodeEvent(event eventRecord) ([]byte, error) { return withTail(event.eventMeta, event.Payload) }

func decodeEvent(value []byte) (eventRecord, error) {
	var event eventRecord

	tail, err := splitTail(value, &event.eventMeta)
	event.Payload = tail

	return event, err
}

// decodeEventHead decodes the metadata of an event without its payload.
func decodeEventHead(value []byte) (eventRecord, error) {
	var event eventRecord

	if len(value) < 4 || int(binary.BigEndian.Uint32(value)) > len(value)-4 {
		return event, errShortRecord
	}

	return event, json.Unmarshal(value[4:4+int(binary.BigEndian.Uint32(value))], &event.eventMeta)
}

// logRecord is the log fields, then its attributes as JSON.
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

// thinProgress is the last event of a level the cleanup has thinned.
type thinProgress struct {
	Rank     byte   `json:"rank"`
	Sequence uint64 `json:"seq"`
}

// timeMark ties an interval to the first sequence number written in it.
type timeMark struct {
	Interval uint64 `json:"interval"`
	First    uint64 `json:"first"`
}

// withTail encodes the JSON head's length, the head, then raw tail bytes.
func withTail(head any, tail []byte) ([]byte, error) {
	encoded, err := json.Marshal(head)

	if err != nil {
		return nil, err
	}

	value := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(encoded)+len(tail)), uint32(len(encoded)))

	return append(append(value, encoded...), tail...), nil
}

// splitTail decodes the head into head and returns a copy of the tail.
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
