package leveldb

import (
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
