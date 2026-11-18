package database

// The engine this binary is built with: the one file that names it. Another
// store is another file like this one under its build tag (//go:build
// kv_<name>), and this one then takes the opposite tag (kv/engine.go).

import (
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	"github.com/devforth/exception-monitoring/internal/storage/kv/leveldb"
)

// engineName is the directory of the engine's data in DATA_DIR.
const engineName = leveldb.Name

func openEngine(dir string) (kv.Engine, error) { return leveldb.Open(dir) }

func openMemoryEngine() kv.Engine { return leveldb.OpenMemory() }
