package leveldb

import (
	"errors"
	"testing"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	"github.com/devforth/exception-monitoring/internal/storage/kv/enginetest"
)

func TestEngine(t *testing.T) {
	t.Run("on disk", func(t *testing.T) { enginetest.Run(t, Open, true) })
	t.Run("in memory", func(t *testing.T) {
		enginetest.Run(t, func(string) (kv.Engine, error) { return OpenMemory(), nil }, false)
	})
}

func TestOpenLocked(t *testing.T) {
	dir := t.TempDir()
	engine, err := Open(dir)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = engine.Close() }()

	if _, err := Open(dir); !errors.Is(err, kv.ErrLocked) {
		t.Fatalf("second open: %v, want kv.ErrLocked", err)
	}
}
