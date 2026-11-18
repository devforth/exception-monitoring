package kv

// Batch collects writes to apply at once (Engine.Write).
// Reads through the batch see its own writes first.
type Batch struct {
	reader Reader
	native WriteBatch
	// latest is the last write of each key; it refers to the slices given, not copies.
	latest map[string]write
	counts map[string]int64 // counter -> change; written by DB.Update
	total  int64            // sum of counts
	after  []func()
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

// Count adds delta to a counter when the batch is written; only DB.Update keeps counters.
func (batch *Batch) Count(counter Key, delta int64) {
	if delta != 0 {
		batch.counts[string(counter)] += delta
		batch.total += delta
	}
}

// Counted is the sum of the counter changes so far: negative when the batch frees bytes.
func (batch *Batch) Counted() int64 { return batch.total }

// AfterWrite runs apply once DB.Update has written the batch, before the next update starts.
func (batch *Batch) AfterWrite(apply func()) { batch.after = append(batch.after, apply) }

// Native is the engine's own batch.
func (batch *Batch) Native() WriteBatch { return batch.native }

// Len returns the number of writes.
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
