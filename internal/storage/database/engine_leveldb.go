package database

// The engine this binary is built with; the only file that names it.

import (
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	"github.com/devforth/exception-monitoring/internal/storage/kv/leveldb"
)

// engineName is the engine's directory in DATA_DIR.
const engineName = leveldb.Name

func openEngine(dir string) (kv.Engine, error) { return leveldb.Open(dir) }

func openEngineReadOnly(dir string) (kv.Engine, error) { return leveldb.OpenReadOnly(dir) }

func openMemoryEngine() kv.Engine { return leveldb.OpenMemory() }
