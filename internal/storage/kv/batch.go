package kv

// Batch collects writes to apply at once (Engine.Write). The writes go
// straight into the engine's own batch, which keeps the one copy of them.
// Reads through the batch see its own writes first, so a record saved twice
// in one batch, or a unique key taken earlier in the batch, is handled like
// a stored one.
type Batch struct {
	reader Reader
	native WriteBatch
	// latest is the last write of each key, for reads through the batch; it
	// refers to the slices given, not copies.
	latest map[string]write
	counts map[string]int64 // counter -> change; written by DB.Update
}

type write struct {
	value   []byte
	deleted bool
}

// NewBatch starts a batch of engine.
func NewBatch(engine Engine) *Batch {
	return &Batch{reader: engine, native: engine.NewWriteBatch(), latest: map[string]write{}, counts: map[string]int64{}}
}

// Put writes key. The batch keeps key and value without copying them: the
// caller must not change them afterwards.
func (batch *Batch) Put(key, value []byte) {
	batch.native.Put(key, value)
	batch.latest[string(key)] = write{value: value}
}

// Delete deletes key.
func (batch *Batch) Delete(key []byte) {
	batch.native.Delete(key)
	batch.latest[string(key)] = write{deleted: true}
}

// Count adds delta to a counter (DB.Counter) when the batch is written.
// Only batches of DB.Update keep counters.
func (batch *Batch) Count(counter Key, delta int64) {
	if delta != 0 {
		batch.counts[string(counter)] += delta
	}
}

// Native is the engine's own batch, for the engine that made it (Write).
func (batch *Batch) Native() WriteBatch { return batch.native }

// Len is the number of writes.
func (batch *Batch) Len() int { return batch.native.Len() }

// Get reads key as it will be once the batch is written.
func (batch *Batch) Get(key []byte) ([]byte, error) {
	if latest, ok := batch.latest[string(key)]; ok {
		if latest.deleted {
			return nil, ErrNotFound
		}

		return latest.value, nil
	}

	return batch.reader.Get(key)
}

// Iterate walks the stored keys only: writes of the batch are not seen.
func (batch *Batch) Iterate(lower, upper []byte) Iterator {
	return batch.reader.Iterate(lower, upper)
}
