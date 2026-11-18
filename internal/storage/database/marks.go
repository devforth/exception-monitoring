package database

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// Time marks tie sequence numbers to receive times, once per markInterval,
// so date ranges become sequence ranges. They are also kept in memory.
const (
	markInterval = 10 * time.Minute
	// markSlack covers records whose SDK time differs from the receive time.
	markSlack = time.Hour
)

func markOf(at time.Time) uint64 { return uint64(at.Unix() / int64(markInterval/time.Second)) }

func (database *Store) loadMarks() error {
	marks, err := database.collections.marks.Primary().All(database.db)
	database.marks = marks

	return err
}

// mark adds the mark of the current interval once; first is the batch's first sequence number.
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

// sequenceFrom returns the first sequence of at's interval, or of the next one marked:
// everything before it arrived before that interval. MaxUint64 when nothing arrived since.
func (database *Store) sequenceFrom(at time.Time) uint64 {
	database.marksMutex.RLock()

	defer database.marksMutex.RUnlock()

	index, _ := slices.BinarySearchFunc(database.marks, markOf(at), compareMark)

	if index >= len(database.marks) {
		return math.MaxUint64
	}

	return database.marks[index].First
}

// sequenceAt returns the first sequence of the latest mark at or before at, or 0.
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

// sequenceAfter returns the first sequence of the first mark after at's interval, or 0.
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

// sequenceOf returns the sequence number that ends an event or log key.
func sequenceOf(key kv.Key) uint64 {
	decoder := kv.Decode(key, len(key)-8)

	return decoder.U64()
}
