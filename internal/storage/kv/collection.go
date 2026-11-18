package kv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Definition declares a collection: name, primary key, indexes and encoding.
type Definition[T any] struct {
	Name    string
	Key     func(T) Key
	Indexes []Index[T]
	// Encode and Decode default to JSON.
	Encode func(T) ([]byte, error)
	Decode func([]byte) (T, error)
	// Counter, when set, counts the bytes a record takes, index keys included.
	Counter func(T) Key
}

// Index declares an index; its keys hold the record's primary key as their value.
type Index[T any] struct {
	Name string
	// Key returns nil to leave the record out. A non-unique index key must
	// still be unique per record: end it with the record's own key parts.
	Key func(T) Key
	// Unique refuses a key another record holds (ErrConflict).
	Unique bool
}

// Collection is built with Define.
type Collection[T any] struct {
	name       string
	prefix     Key
	definition Definition[T]
	indexes    map[string]*index[T]
	order      []*index[T]
}

type index[T any] struct {
	Index[T]
	prefix Key
}

// Define registers a collection under "<name>/" and its indexes under "<name>.<index>/".
func Define[T any](schema *Schema, definition Definition[T]) *Collection[T] {
	if definition.Encode == nil {
		definition.Encode = func(record T) ([]byte, error) { return json.Marshal(record) }
	}

	if definition.Decode == nil {
		definition.Decode = func(value []byte) (T, error) {
			var record T

			return record, json.Unmarshal(value, &record)
		}
	}

	collection := &Collection[T]{
		name: definition.Name, prefix: Key(definition.Name + "/"), definition: definition, indexes: map[string]*index[T]{},
	}
	schema.register(string(collection.prefix), "collection "+definition.Name)

	for _, declared := range definition.Indexes {
		prefix := Key(definition.Name + "." + declared.Name + "/")
		schema.register(string(prefix), "index "+definition.Name+"."+declared.Name)

		entry := &index[T]{Index: declared, prefix: prefix}
		collection.indexes[declared.Name] = entry
		collection.order = append(collection.order, entry)
	}

	return collection
}

// KeyRange covers the keys of the collection and its indexes, and nothing else.
func (collection *Collection[T]) KeyRange() (lower, upper []byte) {
	return []byte(collection.name + "."), prefixEnd(collection.prefix)
}

// Name returns the collection's name.
func (collection *Collection[T]) Name() string { return collection.name }

// KeyOf returns the primary key of record, without the prefix.
func (collection *Collection[T]) KeyOf(record T) Key { return collection.definition.Key(record) }

func (collection *Collection[T]) primaryKey(key Key) []byte {
	return append(bytes.Clone(collection.prefix), key...)
}

func (collection *Collection[T]) indexKeys(record T) []indexKey {
	keys := make([]indexKey, 0, len(collection.order))

	for _, entry := range collection.order {
		if key := entry.Key(record); key != nil {
			keys = append(keys, indexKey{key: append(bytes.Clone(entry.prefix), key...), index: entry.Name, unique: entry.Unique})
		}
	}

	return keys
}

type indexKey struct {
	key    []byte
	index  string
	unique bool
}

// ErrConflict means a unique index already holds the key for another record.
type ErrConflict struct {
	Collection, Index string
}

func (err ErrConflict) Error() string {
	return fmt.Sprintf("kv: %s.%s is already taken", err.Collection, err.Index)
}

// Get reads the record under key, or returns ErrNotFound.
func (collection *Collection[T]) Get(reader Reader, key Key) (T, error) {
	value, err := reader.Get(collection.primaryKey(key))

	if err != nil {
		var zero T

		return zero, err
	}

	return collection.definition.Decode(value)
}

// GetBy reads the record a unique index points to, or returns ErrNotFound.
func (collection *Collection[T]) GetBy(reader Reader, indexName string, key Key) (T, error) {
	entry := collection.index(indexName)
	primary, err := reader.Get(append(bytes.Clone(entry.prefix), key...))

	if err != nil {
		var zero T

		return zero, err
	}

	value, err := reader.Get(primary)

	if err != nil {
		var zero T

		return zero, err
	}

	return collection.definition.Decode(value)
}

// Taken reports whether a unique index holds key.
func (collection *Collection[T]) Taken(reader Reader, indexName string, key Key) (bool, error) {
	_, err := reader.Get(append(bytes.Clone(collection.index(indexName).prefix), key...))

	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}

	return true, nil
}

func (collection *Collection[T]) index(name string) *index[T] {
	entry, ok := collection.indexes[name]

	if !ok {
		panic(fmt.Sprintf("kv: collection %s has no index %q", collection.name, name))
	}

	return entry
}

// Insert writes a record that is not stored yet; use Save to replace one.
func (collection *Collection[T]) Insert(batch *Batch, record T) error {
	_, err := collection.Write(batch, record, nil)

	return err
}

// Save writes a record, replacing the stored one and its old index keys.
func (collection *Collection[T]) Save(batch *Batch, record T) error {
	key := collection.primaryKey(collection.KeyOf(record))
	value, err := batch.Get(key)

	switch {
	case errors.Is(err, ErrNotFound):
		return collection.Insert(batch, record)
	case err != nil:
		return err
	}

	old, err := collection.definition.Decode(value)

	if err != nil {
		return fmt.Errorf("kv: %s %x: %w", collection.name, key, err)
	}

	_, err = collection.Write(batch, record, &Entry[T]{Record: old, Size: len(value)})

	return err
}

// Write writes record over old (nil when none is stored) without reading the
// stored version, for callers that keep it in memory.
func (collection *Collection[T]) Write(batch *Batch, record T, old *Entry[T]) (Entry[T], error) {
	key := collection.primaryKey(collection.KeyOf(record))
	value, err := collection.definition.Encode(record)

	if err != nil {
		return Entry[T]{}, fmt.Errorf("kv: %s: %w", collection.name, err)
	}

	keys := collection.indexKeys(record)

	for _, entry := range keys {
		if !entry.unique {
			continue
		}

		holder, err := batch.Get(entry.key)

		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return Entry[T]{}, err
		case !bytes.Equal(holder, key):
			return Entry[T]{}, ErrConflict{Collection: collection.name, Index: entry.index}
		}
	}

	if old != nil {
		collection.uncount(batch, old.Record, len(key), old.Size)

		kept := map[string]bool{}

		for _, entry := range keys {
			kept[string(entry.key)] = true
		}

		for _, entry := range collection.indexKeys(old.Record) {
			if !kept[string(entry.key)] {
				batch.Delete(entry.key)
			}
		}
	}

	batch.Put(key, value)

	for _, entry := range keys {
		batch.Put(entry.key, key)
	}

	collection.count(batch, record, recordSize(len(key), len(value), keys))

	return Entry[T]{Key: key[len(collection.prefix):], Record: record, Size: len(value)}, nil
}

// Entry is a stored record with its key (without the prefix) and value size.
type Entry[T any] struct {
	Key    Key
	Record T
	Size   int
}

// Delete deletes record with its index keys; DeleteEntry avoids re-encoding it.
func (collection *Collection[T]) Delete(batch *Batch, record T) error {
	size := 0

	if collection.definition.Counter != nil {
		value, err := collection.definition.Encode(record)

		if err != nil {
			return fmt.Errorf("kv: %s: %w", collection.name, err)
		}

		size = len(value)
	}

	collection.remove(batch, record, size)

	return nil
}

// DeleteEntry deletes a record read by a walk.
func (collection *Collection[T]) DeleteEntry(batch *Batch, entry Entry[T]) {
	collection.remove(batch, entry.Record, entry.Size)
}

func (collection *Collection[T]) remove(batch *Batch, record T, valueSize int) {
	key := collection.primaryKey(collection.KeyOf(record))
	batch.Delete(key)

	for _, entry := range collection.indexKeys(record) {
		batch.Delete(entry.key)
	}

	collection.uncount(batch, record, len(key), valueSize)
}

// DeleteKey deletes the record under key; false when there is none.
func (collection *Collection[T]) DeleteKey(batch *Batch, key Key) (bool, error) {
	value, err := batch.Get(collection.primaryKey(key))

	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}

	record, err := collection.definition.Decode(value)

	if err != nil {
		return false, fmt.Errorf("kv: %s: %w", collection.name, err)
	}

	collection.remove(batch, record, len(value))

	return true, nil
}

// recordSize counts the key, value and each index key with its value.
func recordSize(keySize, valueSize int, keys []indexKey) int {
	size := keySize + valueSize

	for _, entry := range keys {
		size += len(entry.key) + keySize
	}

	return size
}

func (collection *Collection[T]) count(batch *Batch, record T, size int) {
	if collection.definition.Counter != nil {
		batch.Count(collection.definition.Counter(record), int64(size))
	}
}

func (collection *Collection[T]) uncount(batch *Batch, record T, keySize, valueSize int) {
	if collection.definition.Counter != nil {
		batch.Count(collection.definition.Counter(record), -int64(recordSize(keySize, valueSize, collection.indexKeys(record))))
	}
}
