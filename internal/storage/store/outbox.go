package store

import (
	"time"
)

// Outbox statuses of a notification message.
const (
	OutboxPending = "pending"
	OutboxSent    = "sent"
	OutboxFailed  = "failed"
)

// OutboxMessage is one message waiting to be sent to one channel. The store
// keeps the payloads opaque; package notify encodes and decodes them.
type OutboxMessage struct {
	ID           int64
	Channel      string
	Kind         string // "notification" or "summary"
	Payload      []byte
	EventPayload []byte // the full event JSON for formats with details; may be empty
	Attempts     int
	CreatedAt    time.Time
}

// OutboxRecord is a row of the outbox as the delivery log shows it.
type OutboxRecord struct {
	ID            int64
	Channel       string // the connection id
	Kind          string // "notification" or "summary"
	Payload       []byte
	Status        string
	Attempts      int
	LastError     string
	CreatedAt     time.Time
	NextAttemptAt time.Time
	UpdatedAt     time.Time
}

// OutboxFilter selects outbox rows, newest first. Zero values mean "any".
type OutboxFilter struct {
	Channel string
	Status  string
	Limit   int
	Offset  int
}

// DeliveryHealth sums up the delivery of one connection: whether its last
// attempts fail, for the notifications list.
type DeliveryHealth struct {
	Pending     int
	Retrying    int // pending after a failed attempt
	Failed      int // given up
	LastSentAt  time.Time
	LastError   string
	LastErrorAt time.Time
}

// Failing: messages are waiting for a retry, or the latest outcome was a failure.
func (health DeliveryHealth) Failing() bool {
	return health.Retrying > 0 || (!health.LastErrorAt.IsZero() && health.LastErrorAt.After(health.LastSentAt))
}
