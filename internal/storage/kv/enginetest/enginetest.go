// Package enginetest is the test every adapter of kv.Engine must pass: an
// adapter that passes it works under the layer, with no need to compare it
// with the adapters already there.
package enginetest

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// Open opens (or creates) an engine in dir.
type Open func(dir string) (kv.Engine, error)

// Run runs the test on engines made by open; persistent says the data
// outlives a Close (an engine on disk), which is then checked too.
func Run(t *testing.T, open Open, persistent bool) {
	t.Helper()

	start := func(t *testing.T) kv.Engine {
		t.Helper()

		engine, err := open(t.TempDir())

		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = engine.Close() })

		return engine
	}

	t.Run("missing key", func(t *testing.T) {
		if _, err := start(t).Get([]byte("none")); !errors.Is(err, kv.ErrNotFound) {
			t.Errorf("got %v, want kv.ErrNotFound", err)
		}
	})

	t.Run("batch writes and deletes", func(t *testing.T) {
		engine := start(t)
		write(t, engine, map[string]string{"a": "1", "b": "2", "c": "3"})

		batch := kv.NewBatch(engine)
		batch.Delete([]byte("a"))
		batch.Put([]byte("d"), []byte("4"))
		batch.Delete([]byte("d")) // written and deleted in one batch

		if batch.Len() != 3 {
			t.Errorf("batch length %d", batch.Len())
		}

		if err := engine.Write(batch, true); err != nil {
			t.Fatal(err)
		}

		expect(t, engine, map[string]string{"b": "2", "c": "3"}, "a", "d")
	})

	t.Run("order and bounds", func(t *testing.T) {
		engine := start(t)
		keys := []string{"", "\x00", "a", "a\x00", "ab", "a\xff", "b", "\xff", "\xff\xff"} // byte order: 0x62 < 0xff
		values := map[string]string{}

		for index, key := range keys {
			values[key] = fmt.Sprint(index)
		}

		write(t, engine, values)

		walk := func(lower, upper []byte, newest bool) []string {
			t.Helper()

			iterator := engine.Iterate(lower, upper)

			var seen []string

			move, valid := iterator.Next, iterator.First()

			if newest {
				move, valid = iterator.Prev, iterator.Last()
			}

			for ; valid; valid = move() {
				seen = append(seen, string(iterator.Key()))

				if string(iterator.Value()) != values[string(iterator.Key())] {
					t.Errorf("value of %q: %q", iterator.Key(), iterator.Value())
				}
			}

			if err := iterator.Close(); err != nil {
				t.Fatal(err)
			}

			return seen
		}

		for name, test := range map[string]struct {
			lower, upper []byte
			newest       bool
			want         []string
		}{
			"all":                {nil, nil, false, keys},
			"all, last first":    {nil, nil, true, reversed(keys)},
			"from a, before b":   {[]byte("a"), []byte("b"), false, []string{"a", "a\x00", "ab", "a\xff"}},
			"from a, last first": {[]byte("a"), []byte("b"), true, []string{"a\xff", "ab", "a\x00", "a"}},
			"empty range":        {[]byte("c"), []byte("d"), false, nil},
		} {
			if got := walk(test.lower, test.upper, test.newest); fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Errorf("%s: %q, want %q", name, got, test.want)
			}
		}

		iterator := engine.Iterate([]byte("a"), []byte("b"))

		if !iterator.Seek([]byte("a\x01")) || string(iterator.Key()) != "ab" {
			t.Errorf("seek: %q", iterator.Key())
		}

		if iterator.Seek([]byte("b")) {
			t.Errorf("seek past the upper bound found %q", iterator.Key())
		}

		_ = iterator.Close()
	})

	t.Run("snapshot", func(t *testing.T) {
		engine := start(t)
		write(t, engine, map[string]string{"a": "old"})

		snapshot, err := engine.Snapshot()

		if err != nil {
			t.Fatal(err)
		}

		defer snapshot.Release()

		write(t, engine, map[string]string{"a": "new", "b": "added"})
		expect(t, snapshot, map[string]string{"a": "old"}, "b")
		expect(t, engine, map[string]string{"a": "new", "b": "added"})
	})

	t.Run("values belong to the caller", func(t *testing.T) {
		engine := start(t)
		value := []byte("value")
		batch := kv.NewBatch(engine)
		batch.Put([]byte("k"), value)

		if err := engine.Write(batch, true); err != nil {
			t.Fatal(err)
		}

		read, _ := engine.Get([]byte("k"))
		read[0] = 'X'

		expect(t, engine, map[string]string{"k": "value"})
	})

	t.Run("size and compaction", func(t *testing.T) {
		engine := start(t)
		values := map[string]string{}

		for index := range 2000 {
			values[fmt.Sprintf("key-%05d", index)] = string(bytes.Repeat([]byte{byte(index)}, 100))
		}

		write(t, engine, values)

		if size, err := engine.Size(); err != nil || size <= 0 {
			t.Errorf("size %d, %v", size, err)
		}

		batch := kv.NewBatch(engine)

		for key := range values {
			batch.Delete([]byte(key))
		}

		if err := engine.Write(batch, true); err != nil {
			t.Fatal(err)
		}

		if err := engine.CompactRange(nil, nil); err != nil {
			t.Fatal(err)
		}

		expect(t, engine, nil, "key-00000", "key-01999")
	})

	if !persistent {
		return
	}

	t.Run("data outlives a close", func(t *testing.T) {
		dir := t.TempDir()
		engine, err := open(dir)

		if err != nil {
			t.Fatal(err)
		}

		write(t, engine, map[string]string{"kept": "yes"})

		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}

		if engine, err = open(dir); err != nil {
			t.Fatal(err)
		}

		defer func() { _ = engine.Close() }()

		expect(t, engine, map[string]string{"kept": "yes"})
	})
}

func write(t *testing.T, engine kv.Engine, values map[string]string) {
	t.Helper()

	batch := kv.NewBatch(engine)

	for key, value := range values {
		batch.Put([]byte(key), []byte(value))
	}

	if err := engine.Write(batch, true); err != nil {
		t.Fatal(err)
	}
}

// expect checks that reader holds values and none of missing.
func expect(t *testing.T, reader kv.Reader, values map[string]string, missing ...string) {
	t.Helper()

	for key, want := range values {
		if got, err := reader.Get([]byte(key)); err != nil || string(got) != want {
			t.Errorf("%q: %q, %v; want %q", key, got, err, want)
		}
	}

	for _, key := range missing {
		if _, err := reader.Get([]byte(key)); !errors.Is(err, kv.ErrNotFound) {
			t.Errorf("%q: %v, want kv.ErrNotFound", key, err)
		}
	}
}

func reversed(keys []string) []string {
	result := make([]string, len(keys))

	for index, key := range keys {
		result[len(keys)-1-index] = key
	}

	return result
}
