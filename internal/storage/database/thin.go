package database

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// Thinning keeps of an old event only what describes the error, without
// local variables, and marks it as thinned.

// thinnedFields are the top-level fields a thinned event keeps.
var thinnedFields = map[string]bool{
	"event_id": true, "timestamp": true, "level": true, "platform": true, "logger": true, "transaction": true,
	"release": true, "environment": true, "message": true, "logentry": true, "exception": true, "fingerprint": true,
	"stacktrace": true, "threads": true,
}

var stackFields = []string{"exception", "stacktrace", "threads"}

// thinPayload keeps thinnedFields without local variables; it returns the
// payload as is when that is not smaller.
func thinPayload(payload []byte) []byte {
	var event map[string]json.RawMessage

	if json.Unmarshal(payload, &event) != nil {
		return payload
	}

	for field := range event {
		if !thinnedFields[field] {
			delete(event, field)
		}
	}

	for _, field := range stackFields {
		if stack, ok := event[field]; ok {
			event[field] = withoutVars(stack)
		}
	}

	thinned, err := marshalPlain(event)

	if err != nil || len(thinned) >= len(payload) {
		return payload
	}

	return thinned
}

// withoutVars drops "vars" from every object inside a stack.
func withoutVars(raw json.RawMessage) json.RawMessage {
	var value any

	if json.Unmarshal(raw, &value) != nil {
		return raw
	}

	var strip func(any)

	strip = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			delete(typed, "vars")

			for _, child := range typed {
				strip(child)
			}
		case []any:
			for _, child := range typed {
				strip(child)
			}
		}
	}

	strip(value)

	stripped, err := marshalPlain(value)

	if err != nil {
		return raw
	}

	return stripped
}

// marshalPlain is json.Marshal without escaping <, > and &, which would make stack traces longer.
func marshalPlain(value any) ([]byte, error) {
	var buffer bytes.Buffer

	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(value); err != nil {
		return nil, err
	}

	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// thin writes an event thinned; one that would not get smaller stays as it is.
func (writer *batchWriter) thin(entry kv.Entry[eventRecord]) error {
	record := entry.Record
	record.Payload = thinPayload(record.Payload)

	if len(record.Payload) == len(entry.Record.Payload) {
		return nil
	}

	record.Thinned = true
	writer.count(record.ProjectID).Thinned++

	_, err := writer.database.events.Write(writer.batch, record, &entry)

	return err
}

// thinBefore thins events received before cutoff. How far each level got is
// kept between runs, so old events are read once. Position: rank.
func (database *Store) thinBefore(cutoff time.Time) cleanupBatch {
	return func(writer *batchWriter, cursor *cleanupCursor) (bool, error) {
		start := kv.Decode(cursor.position, 0).U8()

		for _, rank := range allRanks {
			if rank < start {
				continue
			}

			var from uint64

			switch progress, err := database.thin.Get(database.db, kv.K(kv.U8(rank))); {
			case err == nil:
				from = progress.Sequence + 1
			case !errors.Is(err, kv.ErrNotFound):
				return false, err
			}

			full, moved := false, false
			last := thinProgress{Rank: rank}

			err := database.events.By("age", kv.U8(rank)).From(kv.U64(from)).Each(database.db, func(entry kv.Entry[eventRecord]) (bool, error) {
				if !entry.Record.ReceivedAt.Before(cutoff) {
					return false, nil
				}

				if full = cursor.full(); full {
					return false, nil
				}

				cursor.records++
				last.Sequence, moved = entry.Record.Sequence, true

				if entry.Record.Thinned {
					return true, nil
				}

				cursor.decoded += len(entry.Record.Payload)

				return true, writer.thin(entry)
			})

			if err == nil && moved {
				err = database.thin.Save(writer.batch, last)
			}

			if err != nil || full {
				cursor.position = kv.K(kv.U8(rank))

				return false, err
			}
		}

		return true, nil
	}
}
