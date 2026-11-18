package kv

import "bytes"

// Batch collects writes to apply at once (Engine.Write). Reads through it
// see its own writes first, so a record saved twice in one batch, or a
// unique key taken earlier in the batch, is handled like a stored one.
type Batch struct {
	reader     Reader
	operations []operation
	latest     map[string]int   // key -> index of its latest operation
	counts     map[string]int64 // counter -> change; written by DB.Update
}

type operation struct {
	key     []byte
	value   []byte
	deleted bool
}

// NewBatch starts a batch whose reads fall through to reader.
func NewBatch(reader Reader) *Batch {
	return &Batch{reader: reader, latest: map[string]int{}, counts: map[string]int64{}}
}

// Count adds delta to a counter (DB.Counter) when the batch is written.
// Only batches of DB.Update keep counters.
func (batch *Batch) Count(counter Key, delta int64) {
	if delta != 0 {
		batch.counts[string(counter)] += delta
	}
}

// Put writes key. The batch keeps its own copies of key and value.
func (batch *Batch) Put(key, value []byte) {
	batch.add(operation{key: bytes.Clone(key), value: bytes.Clone(value)})
}

// Delete deletes key.
func (batch *Batch) Delete(key []byte) {
	batch.add(operation{key: bytes.Clone(key), deleted: true})
}

func (batch *Batch) add(entry operation) {
	batch.latest[string(entry.key)] = len(batch.operations)
	batch.operations = append(batch.operations, entry)
}

// Len is the number of writes.
func (batch *Batch) Len() int { return len(batch.operations) }

// Get reads key as it will be once the batch is written.
func (batch *Batch) Get(key []byte) ([]byte, error) {
	if index, ok := batch.latest[string(key)]; ok {
		if batch.operations[index].deleted {
			return nil, ErrNotFound
		}

		return bytes.Clone(batch.operations[index].value), nil
	}

	return batch.reader.Get(key)
}

// Iterate walks the stored keys only: writes of the batch are not seen.
func (batch *Batch) Iterate(lower, upper []byte) Iterator {
	return batch.reader.Iterate(lower, upper)
}
