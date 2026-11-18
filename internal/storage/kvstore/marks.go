package kvstore

import (
	"cmp"
	"slices"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// Time marks tie sequence numbers to the time records were received: at the
// first batch of every markInterval, the writer stores a mark with the first
// sequence number that batch may use. Every record written later has a
// higher number, every one before a lower, so a list limited by dates
// starts and stops by sequence number instead of reading every record, and
// the cleanup can cut whole days. The marks are also kept in memory (a few
// thousand a month).
const (
	markInterval = 10 * time.Minute
	// markSlack covers records whose own time (the SDK's clock) differs
	// from the time they arrived.
	markSlack = time.Hour
)

func markOf(at time.Time) uint64 { return uint64(at.Unix() / int64(markInterval/time.Second)) }

func (database *Store) loadMarks() error {
	marks, err := database.collections.marks.Primary().All(database.db)
	database.marks = marks

	return err
}

// mark adds the time mark of the interval now to the batch, once per
// interval; first is the first sequence number the batch may use.
func (writer *batchWriter) mark(now time.Time, first uint64) error {
	interval := markOf(now)

	writer.database.marksMutex.RLock()
	marked := len(writer.database.marks) > 0 && writer.database.marks[len(writer.database.marks)-1].Interval >= interval
	writer.database.marksMutex.RUnlock()

	if marked {
		return nil
	}

	writer.newMark = &timeMark{Interval: interval, First: first}

	return writer.database.collections.marks.Insert(writer.batch, *writer.newMark)
}

// addMark puts a committed mark in memory.
func (database *Store) addMark(mark timeMark) {
	database.marksMutex.Lock()
	defer database.marksMutex.Unlock()

	database.marks = append(database.marks, mark)
}

// sequenceAt is the first sequence number of the latest mark at or before
// at: records received before that mark's interval have lower numbers. 0
// when there is no such mark.
func (database *Store) sequenceAt(at time.Time) uint64 {
	database.marksMutex.RLock()

	defer database.marksMutex.RUnlock()

	index, found := slices.BinarySearchFunc(database.marks, markOf(at), compareMark)

	switch {
	case found:
		return database.marks[index].First
	case index == 0:
		return 0
	default:
		return database.marks[index-1].First
	}
}

// sequenceAfter is the first sequence number of the first mark after the
// interval of at: records with that number or higher were received after
// at. 0 when there is no such mark yet.
func (database *Store) sequenceAfter(at time.Time) uint64 {
	database.marksMutex.RLock()

	defer database.marksMutex.RUnlock()

	index, found := slices.BinarySearchFunc(database.marks, markOf(at), compareMark)

	if found {
		index++
	}

	if index >= len(database.marks) {
		return 0
	}

	return database.marks[index].First
}

func compareMark(mark timeMark, interval uint64) int { return cmp.Compare(mark.Interval, interval) }

// sequenceOf is the sequence number that ends a key of events and logs.
func sequenceOf(key kv.Key) uint64 {
	decoder := kv.Decode(key, len(key)-8)

	return decoder.U64()
}
