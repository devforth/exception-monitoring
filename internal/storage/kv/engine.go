package kv

import "errors"

// Engine is the ordered key-value store under the layer, implemented by an
// adapter package under kv/ (kv/leveldb) that must pass enginetest.
type Engine interface {
	Reader
	NewWriteBatch() WriteBatch
	// Write applies the batch atomically; sync waits until it is on disk.
	Write(batch *Batch, sync bool) error
	// Snapshot must be released.
	Snapshot() (Snapshot, error)
	// CompactRange frees the space of deleted keys in [lower, upper); nil bounds are open.
	CompactRange(lower, upper []byte) error
	Size() (int64, error)
	Close() error
}

// Reader reads keys: an Engine, a Snapshot, or a Batch over one.
type Reader interface {
	// Get returns ErrNotFound for a missing key; the value belongs to the caller.
	Get(key []byte) ([]byte, error)
	// Iterate walks the keys in [lower, upper); nil bounds are open.
	Iterate(lower, upper []byte) Iterator
}

// WriteBatch is an engine's own batch: it copies what it is given once.
type WriteBatch interface {
	Put(key, value []byte)
	Delete(key []byte)
	Len() int
}

// Snapshot is a Reader fixed at one moment.
type Snapshot interface {
	Reader
	Release()
}

// Iterator walks keys in order. Key and Value are valid until the next
// move; copy them to keep them.
type Iterator interface {
	First() bool
	Last() bool
	Next() bool
	Prev() bool
	Key() []byte
	Value() []byte
	Close() error
}

// ErrNotFound is returned for a missing key.
var ErrNotFound = errors.New("kv: not found")

// ErrLocked is returned by an adapter's Open when another process holds the database.
var ErrLocked = errors.New("the database is in use by another process")
