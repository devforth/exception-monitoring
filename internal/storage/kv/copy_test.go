package kv

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"testing"
)

func TestCopy(t *testing.T) {
	from, to := newMemoryEngine(), newMemoryEngine()
	value := bytes.Repeat([]byte("v"), 64<<10)
	batch := NewBatch(from)

	// More than one batch of Copy.
	for index := range 100 {
		batch.Put(fmt.Appendf(nil, "key-%03d", index), value)
	}

	if err := from.Write(batch, false); err != nil {
		t.Fatal(err)
	}

	snapshot, err := from.Snapshot()

	if err != nil {
		t.Fatal(err)
	}

	defer snapshot.Release()

	want := maps.Clone(from.data)
	later := NewBatch(from)
	later.Put([]byte("key-later"), value)

	if err := from.Write(later, false); err != nil {
		t.Fatal(err)
	}

	calls := 0
	keys, total, err := Copy(context.Background(), snapshot, to, func(int64, int64) { calls++ })

	if err != nil {
		t.Fatal(err)
	}

	if keys != 100 || total != int64(100*(len("key-000")+len(value))) || calls < 2 {
		t.Errorf("copied %d keys, %d bytes, %d progress calls", keys, total, calls)
	}

	if !maps.EqualFunc(to.data, want, bytes.Equal) {
		t.Errorf("copy holds %d keys, want the %d of the snapshot", len(to.data), len(want))
	}
}

func TestCopyStopsOnCancel(t *testing.T) {
	from, to := newMemoryEngine(), newMemoryEngine()
	batch := NewBatch(from)
	batch.Put([]byte("key"), []byte("value"))

	if err := from.Write(batch, false); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := Copy(ctx, from, to, nil); err == nil {
		t.Error("copy with a canceled context succeeded")
	}
}
