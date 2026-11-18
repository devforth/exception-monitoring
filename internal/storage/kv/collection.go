package kv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Definition declares a collection: its name (the key prefix), its primary
// key, its indexes, and how its records are stored.
type Definition[T any] struct {
	Name string
	// Key is the primary key of a record, after the collection's prefix.
	Key     func(T) Key
	Indexes []Index[T]
	// Encode and Decode store a record; nil uses JSON.
	Encode func(T) ([]byte, error)
	Decode func([]byte) (T, error)
	// Counter, when set, is the counter that holds the bytes a record takes
	// (its keys and value, index keys included); writes and deletes through
	// the collection keep it up to date (DB.Update).
	Counter func(T) Key
}

// Index declares an index of a collection. Its keys hold the record's
// primary key as their value.
type Index[T any] struct {
	Name string
	// Key is the index key of a record, after the index prefix; nil leaves
	// the record out of the index. The key of a non-unique index must
	// itself be unique per record: end it with the record's own key parts
	// (a sequence number), which also sets the order of the walk.
	Key func(T) Key
	// Unique indexes refuse a key another record holds (ErrConflict).
	Unique bool
}

// Collection is a defined collection: build one with Define.
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

// Define registers a collection and its indexes in schema. The primary keys
// live under "<name>/", an index under "<name>.<index>/".
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

// Name is the collection's name.
func (collection *Collection[T]) Name() string { return collection.name }

// KeyOf is the primary key of record, after the collection's prefix.
func (collection *Collection[T]) KeyOf(record T) Key { return collection.definition.Key(record) }

func (collection *Collection[T]) primaryKey(key Key) []byte {
	return append(bytes.Clone(collection.prefix), key...)
}

// indexKeys are the index keys of record, unique ones marked.
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

// ErrConflict: a unique index already holds the key for another record.
type ErrConflict struct {
	Collection, Index string
}

func (err ErrConflict) Error() string {
	return fmt.Sprintf("kv: %s.%s is already taken", err.Collection, err.Index)
}

// Get reads the record under key (after the collection's prefix), or
// ErrNotFound.
func (collection *Collection[T]) Get(reader Reader, key Key) (T, error) {
	value, err := reader.Get(collection.primaryKey(key))

	if err != nil {
		var zero T

		return zero, err
	}

	return collection.definition.Decode(value)
}

// GetBy reads the record a unique index points to, or ErrNotFound.
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

// Taken reports whether a unique index holds key, through reader (a batch
// sees its own writes).
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

// Insert writes a record that is not stored yet, with its index keys. A
// record already stored under the same key must go through Save, which
// removes its old index keys.
func (collection *Collection[T]) Insert(batch *Batch, record T) error {
	return collection.write(batch, record, nil)
}

// Save writes a record, replacing the stored one under the same key: index
// keys the record no longer has are deleted.
func (collection *Collection[T]) Save(batch *Batch, record T) error {
	key := collection.primaryKey(collection.KeyOf(record))
	value, err := batch.Get(key)

	switch {
	case errors.Is(err, ErrNotFound):
		return collection.write(batch, record, nil)
	case err != nil:
		return err
	}

	old, err := collection.definition.Decode(value)

	if err != nil {
		return fmt.Errorf("kv: %s %x: %w", collection.name, key, err)
	}

	return collection.write(batch, record, &stored[T]{record: old, size: len(value)})
}

// stored is the version of a record being replaced.
type stored[T any] struct {
	record T
	size   int
}

func (collection *Collection[T]) write(batch *Batch, record T, old *stored[T]) error {
	key := collection.primaryKey(collection.KeyOf(record))
	value, err := collection.definition.Encode(record)

	if err != nil {
		return fmt.Errorf("kv: %s: %w", collection.name, err)
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
			return err
		case !bytes.Equal(holder, key):
			return ErrConflict{Collection: collection.name, Index: entry.index}
		}
	}

	if old != nil {
		collection.uncount(batch, old.record, len(key), old.size)

		kept := map[string]bool{}

		for _, entry := range keys {
			kept[string(entry.key)] = true
		}

		for _, entry := range collection.indexKeys(old.record) {
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

	return nil
}

// Entry is a stored record as a walk reads it: its key (after the
// collection's prefix), the record, and the size of its stored value.
type Entry[T any] struct {
	Key    Key
	Record T
	Size   int
}

// Delete deletes record with its index keys. With a counter, the record is
// encoded again to know what it took; DeleteEntry avoids that.
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

// DeleteEntry deletes a record read by a walk, with its index keys.
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

// DeleteKey deletes the record under key with its index keys; false when
// there is none.
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

// recordSize is what a record takes: its key and value, and each index key
// with the primary key as its value.
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

// uncount takes back what a stored record of valueSize bytes was counted.
func (collection *Collection[T]) uncount(batch *Batch, record T, keySize, valueSize int) {
	if collection.definition.Counter != nil {
		batch.Count(collection.definition.Counter(record), -int64(recordSize(keySize, valueSize, collection.indexKeys(record))))
	}
}
