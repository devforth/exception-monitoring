package kv

import (
	"bytes"
	"slices"
	"sort"
	"sync"
)

// memoryEngine is a sorted map, the engine of the layer's own tests: the
// layer is tested apart from any real store (those pass enginetest).
type memoryEngine struct {
	mutex sync.RWMutex
	data  map[string][]byte
}

func newMemoryEngine() *memoryEngine { return &memoryEngine{data: map[string][]byte{}} }

type memoryBatch struct{ writes []memoryWrite }

type memoryWrite struct {
	key, value []byte
	deleted    bool
}

func (batch *memoryBatch) Put(key, value []byte) {
	batch.writes = append(batch.writes, memoryWrite{key: bytes.Clone(key), value: bytes.Clone(value)})
}

func (batch *memoryBatch) Delete(key []byte) {
	batch.writes = append(batch.writes, memoryWrite{key: bytes.Clone(key), deleted: true})
}

func (batch *memoryBatch) Len() int { return len(batch.writes) }

func (engine *memoryEngine) NewWriteBatch() WriteBatch { return &memoryBatch{} }

func (engine *memoryEngine) Write(batch *Batch, _ bool) error {
	engine.mutex.Lock()
	defer engine.mutex.Unlock()

	for _, entry := range batch.Native().(*memoryBatch).writes {
		if entry.deleted {
			delete(engine.data, string(entry.key))
		} else {
			engine.data[string(entry.key)] = entry.value
		}
	}

	return nil
}

func (engine *memoryEngine) Get(key []byte) ([]byte, error) {
	engine.mutex.RLock()

	defer engine.mutex.RUnlock()

	value, ok := engine.data[string(key)]

	if !ok {
		return nil, ErrNotFound
	}

	return bytes.Clone(value), nil
}

func (engine *memoryEngine) Iterate(lower, upper []byte) Iterator {
	engine.mutex.RLock()

	defer engine.mutex.RUnlock()

	it := &memoryIterator{position: -1}

	for key, value := range engine.data {
		if (lower == nil || key >= string(lower)) && (upper == nil || key < string(upper)) {
			it.keys = append(it.keys, key)
			it.values = append(it.values, value)
		}
	}

	order := make([]int, len(it.keys))

	for index := range order {
		order[index] = index
	}

	sort.Slice(order, func(a, b int) bool { return it.keys[order[a]] < it.keys[order[b]] })

	keys, values := slices.Clone(it.keys), slices.Clone(it.values)

	for index, from := range order {
		it.keys[index], it.values[index] = keys[from], values[from]
	}

	return it
}

func (engine *memoryEngine) Snapshot() (Snapshot, error) {
	engine.mutex.RLock()

	defer engine.mutex.RUnlock()

	copied := newMemoryEngine()

	for key, value := range engine.data {
		copied.data[key] = value
	}

	return memorySnapshot{copied}, nil
}

type memorySnapshot struct{ *memoryEngine }

func (memorySnapshot) Release() {}

func (engine *memoryEngine) CompactRange(_, _ []byte) error { return nil }

func (engine *memoryEngine) Size() (int64, error) {
	engine.mutex.RLock()

	defer engine.mutex.RUnlock()

	var size int64

	for key, value := range engine.data {
		size += int64(len(key) + len(value))
	}

	return size, nil
}

func (engine *memoryEngine) Close() error { return nil }

type memoryIterator struct {
	keys     []string
	values   [][]byte
	position int
}

func (it *memoryIterator) valid() bool { return it.position >= 0 && it.position < len(it.keys) }

func (it *memoryIterator) First() bool { it.position = 0; return it.valid() }

func (it *memoryIterator) Last() bool { it.position = len(it.keys) - 1; return it.valid() }

func (it *memoryIterator) Seek(key []byte) bool {
	it.position = sort.SearchStrings(it.keys, string(key))

	return it.valid()
}

func (it *memoryIterator) Next() bool { it.position++; return it.valid() }

func (it *memoryIterator) Prev() bool { it.position--; return it.valid() }

func (it *memoryIterator) Key() []byte { return []byte(it.keys[it.position]) }

func (it *memoryIterator) Value() []byte { return it.values[it.position] }

func (it *memoryIterator) Close() error { return nil }
