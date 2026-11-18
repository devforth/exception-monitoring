package kv

import (
	"bytes"
	"errors"
	"fmt"
)

// Query walks the primary keys of a collection or one of its indexes: the
// keys under a prefix, optionally between bounds, in key order or newest
// (last) first. There is no free "where": a query names the index, and
// Where checks each record the walk reads, which the code then shows.
type Query[T any] struct {
	collection   *Collection[T]
	index        *index[T] // nil: the primary keys
	prefix       Key
	lower, upper Key
	newest       bool
	limit        int
	where        func(T) bool
}

// Primary walks the records whose key starts with parts.
func (collection *Collection[T]) Primary(parts ...Part) Query[T] {
	return Query[T]{collection: collection, prefix: collection.prefix.With(parts...)}
}

// By walks the index name, the keys that start with parts.
func (collection *Collection[T]) By(name string, parts ...Part) Query[T] {
	entry := collection.index(name)

	return Query[T]{collection: collection, index: entry, prefix: entry.prefix.With(parts...)}
}

// From keeps the keys at or after the prefix followed by parts.
func (query Query[T]) From(parts ...Part) Query[T] {
	query.lower = query.prefix.With(parts...)

	return query
}

// Before keeps the keys before the prefix followed by parts.
func (query Query[T]) Before(parts ...Part) Query[T] {
	query.upper = query.prefix.With(parts...)

	return query
}

// Newest walks from the last key to the first.
func (query Query[T]) Newest() Query[T] {
	query.newest = true

	return query
}

// Limit stops after n records (0: no limit).
func (query Query[T]) Limit(n int) Query[T] {
	query.limit = n

	return query
}

// Where keeps the records match accepts. It is checked per record read, so
// the walk may read many records for a few matches.
func (query Query[T]) Where(match func(T) bool) Query[T] {
	query.where = match

	return query
}

// bounds are the keys the walk covers.
func (query Query[T]) bounds() ([]byte, []byte) {
	lower, upper := []byte(query.prefix), prefixEnd(query.prefix)

	if query.lower != nil && bytes.Compare(query.lower, lower) > 0 {
		lower = query.lower
	}

	if query.upper != nil && (upper == nil || bytes.Compare(query.upper, upper) < 0) {
		upper = query.upper
	}

	return lower, upper
}

// Each calls visit with each record of the walk until it returns false or
// the limit is reached. A record an index points to but that is gone is
// skipped (Check reports it).
func (query Query[T]) Each(reader Reader, visit func(Entry[T]) (bool, error)) error {
	cursor := query.Open(reader)

	for {
		entry, ok := cursor.Next()

		if !ok {
			return cursor.Close()
		}

		more, err := visit(entry)

		if err != nil || !more {
			return errors.Join(err, cursor.Close())
		}
	}
}

// Cursor reads a walk one record at a time, for code that merges several
// walks (Each is simpler otherwise).
type Cursor[T any] struct {
	query    Query[T]
	reader   Reader
	iterator Iterator
	started  bool
	found    int
	err      error
}

// Open starts the walk; Close the cursor when done.
func (query Query[T]) Open(reader Reader) *Cursor[T] {
	lower, upper := query.bounds()

	return &Cursor[T]{query: query, reader: reader, iterator: reader.Iterate(lower, upper)}
}

// Next returns the next record of the walk; false at its end or on an
// error (Close returns it).
func (cursor *Cursor[T]) Next() (Entry[T], bool) {
	query, iterator := cursor.query, cursor.iterator

	if cursor.err != nil || (query.limit > 0 && cursor.found >= query.limit) {
		return Entry[T]{}, false
	}

	for {
		var valid bool

		switch {
		case !cursor.started && query.newest:
			valid = iterator.Last()
		case !cursor.started:
			valid = iterator.First()
		case query.newest:
			valid = iterator.Prev()
		default:
			valid = iterator.Next()
		}

		cursor.started = true

		if !valid {
			return Entry[T]{}, false
		}

		entry, ok, err := query.read(cursor.reader, iterator)

		if err != nil {
			cursor.err = err

			return Entry[T]{}, false
		}

		if ok && (query.where == nil || query.where(entry.Record)) {
			cursor.found++

			return entry, true
		}
	}
}

// Close releases the walk and returns its first error.
func (cursor *Cursor[T]) Close() error {
	return errors.Join(cursor.err, cursor.iterator.Close())
}

// read decodes the record at the iterator: its value for the primary keys,
// the record the value points to for an index.
func (query Query[T]) read(reader Reader, iterator Iterator) (Entry[T], bool, error) {
	key, value := iterator.Key(), iterator.Value()

	if query.index != nil {
		primary := bytes.Clone(value)
		stored, err := reader.Get(primary)

		if errors.Is(err, ErrNotFound) {
			return Entry[T]{}, false, nil
		}

		if err != nil {
			return Entry[T]{}, false, err
		}

		key, value = primary, stored
	}

	record, err := query.collection.definition.Decode(value)

	if err != nil {
		return Entry[T]{}, false, fmt.Errorf("kv: %s %x: %w", query.collection.name, key, err)
	}

	return Entry[T]{Key: bytes.Clone(key[len(query.collection.prefix):]), Record: record, Size: len(value)}, true, nil
}

// All returns the records of the walk.
func (query Query[T]) All(reader Reader) ([]T, error) {
	var records []T

	err := query.Each(reader, func(entry Entry[T]) (bool, error) {
		records = append(records, entry.Record)

		return true, nil
	})

	return records, err
}

// First returns the first record of the walk, or ErrNotFound.
func (query Query[T]) First(reader Reader) (Entry[T], error) {
	var first *Entry[T]

	err := query.Limit(1).Each(reader, func(entry Entry[T]) (bool, error) {
		first = &entry

		return false, nil
	})

	if err == nil && first == nil {
		err = ErrNotFound
	}

	if first == nil {
		return Entry[T]{}, err
	}

	return *first, err
}

// Count counts the records of the walk, up to most (0: all); more says
// there are more than most.
func (query Query[T]) Count(reader Reader, most int) (count int, more bool, err error) {
	err = query.Limit(0).Each(reader, func(Entry[T]) (bool, error) {
		if most > 0 && count == most {
			more = true

			return false, nil
		}

		count++

		return true, nil
	})

	return count, more, err
}
