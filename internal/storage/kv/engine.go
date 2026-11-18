package kv

import "errors"

// Engine is the ordered key-value store under the layer. Any store that
// keeps keys sorted can implement it; goleveldb is the one built (package
// kv/leveldb).
//
// An adapter is a package under kv/ that alone imports its store's library
// and implements Engine; package enginetest is the test it must pass. The
// database package picks the adapter with a build tag (one file per adapter,
// //go:build kv_<name>, the default taking the opposite tag): a binary
// carries one adapter, so the default build has no other store or driver.
// Each store keeps its data in its own directory, DATA_DIR/<name>, so
// switching starts empty and never touches the data of another.
// adapters_test.go checks that no other code imports a store's library.
type Engine interface {
	Reader
	// NewWriteBatch starts a batch of the engine's own kind (NewBatch).
	NewWriteBatch() WriteBatch
	// Write applies the batch atomically; sync waits until it is on disk.
	Write(batch *Batch, sync bool) error
	// Snapshot is a consistent view of the store as of now; release it.
	Snapshot() (Snapshot, error)
	// CompactRange rewrites the files that hold keys in [lower, upper), so
	// deleted data gives its space back; nil bounds are open.
	CompactRange(lower, upper []byte) error
	// Size is the space the store takes on disk.
	Size() (int64, error)
	Close() error
}

// Reader reads keys: an Engine, a Snapshot, or a Batch over one.
type Reader interface {
	// Get returns the value of key, or ErrNotFound. The value belongs to
	// the caller.
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
	// Seek moves to the first key at or after key.
	Seek(key []byte) bool
	Next() bool
	Prev() bool
	Key() []byte
	Value() []byte
	// Close releases the iterator and returns its error, if any.
	Close() error
}

// ErrNotFound: the key is not in the store.
var ErrNotFound = errors.New("kv: not found")
