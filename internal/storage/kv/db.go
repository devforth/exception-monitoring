package kv

import (
	"encoding/binary"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
)

// DB is an engine with one writer, counters and sequence numbers on top.
type DB struct {
	Engine

	counterPrefix Key
	sequenceKey   Key

	writeMutex sync.Mutex // one DB.Update at a time

	countersMutex sync.RWMutex
	counters      map[string]int64

	written atomic.Int64

	sequenceMutex sync.Mutex
	next          uint64 // the next sequence number
	reserved      uint64 // stored; numbers below it may be handed out
}

// sequenceBlock numbers are reserved at a time; after a crash the rest of the block is skipped.
const sequenceBlock = 10_000

// OpenDB opens a DB on engine and reserves its prefixes in schema.
func OpenDB(engine Engine, schema *Schema) (*DB, error) {
	meta := schema.Reserve("meta")
	db := &DB{
		Engine: engine, counterPrefix: schema.Reserve("count"), sequenceKey: meta.With(Text("sequence")),
		counters: map[string]int64{},
	}

	value, err := engine.Get(db.sequenceKey)

	switch {
	case errors.Is(err, ErrNotFound):
		db.next = 1
	case err != nil:
		return nil, err
	default:
		db.next = binary.BigEndian.Uint64(value)
	}

	db.reserved = db.next

	iterator := engine.Iterate(db.counterPrefix, prefixEnd(db.counterPrefix))

	for valid := iterator.First(); valid; valid = iterator.Next() {
		db.counters[string(iterator.Key()[len(db.counterPrefix):])] = int64(binary.BigEndian.Uint64(iterator.Value()))
	}

	if err := iterator.Close(); err != nil {
		return nil, err
	}

	return db, nil
}

// Next returns a sequence number larger than any before, also across restarts.
func (db *DB) Next() (uint64, error) {
	db.sequenceMutex.Lock()
	defer db.sequenceMutex.Unlock()

	if db.next >= db.reserved {
		reserved := db.next + sequenceBlock
		batch := NewBatch(db.Engine)
		batch.Put(db.sequenceKey, binary.BigEndian.AppendUint64(nil, reserved))

		if err := db.Write(batch, true); err != nil {
			return 0, err
		}

		db.reserved = reserved
	}

	number := db.next
	db.next++

	return number, nil
}

// Peek returns the number Next would return, without taking it.
func (db *DB) Peek() uint64 {
	db.sequenceMutex.Lock()
	defer db.sequenceMutex.Unlock()

	return db.next
}

// Update runs change on a new batch and writes it with its counters, one at a time.
// Nothing is written when change fails.
func (db *DB) Update(sync bool, change func(batch *Batch) error) error {
	db.writeMutex.Lock()
	defer db.writeMutex.Unlock()

	batch := NewBatch(db.Engine)

	if err := change(batch); err != nil {
		return err
	}

	totals := map[string]int64{}

	db.countersMutex.RLock()

	for counter, delta := range batch.counts {
		totals[counter] = db.counters[counter] + delta
	}

	db.countersMutex.RUnlock()

	for counter, total := range totals {
		key := db.counterPrefix.With()
		key = append(key, counter...)

		if total == 0 {
			batch.Delete(key)
		} else {
			batch.Put(key, binary.BigEndian.AppendUint64(nil, uint64(total)))
		}
	}

	if err := db.Write(batch, sync); err != nil {
		return err
	}

	for _, delta := range batch.counts {
		db.written.Add(max(delta, 0))
	}

	db.countersMutex.Lock()

	for counter, total := range totals {
		if total == 0 {
			delete(db.counters, counter)
		} else {
			db.counters[counter] = total
		}
	}

	db.countersMutex.Unlock()

	for _, apply := range batch.after {
		apply()
	}

	return nil
}

// Written returns the bytes updates have added to counters since the DB was opened.
func (db *DB) Written() int64 { return db.written.Load() }

// Counter returns the value of a counter.
func (db *DB) Counter(counter Key) int64 {
	db.countersMutex.RLock()

	defer db.countersMutex.RUnlock()

	return db.counters[string(counter)]
}

// Counters returns a copy of every counter.
func (db *DB) Counters() map[string]int64 {
	db.countersMutex.RLock()

	defer db.countersMutex.RUnlock()

	return maps.Clone(db.counters)
}
