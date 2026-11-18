// Package storetest opens a store for tests: SQLite, or with STORE=kv the
// database package for the parts written on it and SQLite for the rest, so
// the same tests check both while the storage moves (docs/storage-plan.md).
package storetest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/devforth/exception-monitoring/internal/storage/database"
	"github.com/devforth/exception-monitoring/internal/storage/sqlite"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Open opens a new store in a temporary directory, closed with the test.
func Open(t testing.TB) store.Store {
	t.Helper()

	dir := t.TempDir()
	relational, err := sqlite.Open(context.Background(), filepath.Join(dir, "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	if !KV() {
		t.Cleanup(func() { _ = relational.Close() })

		return relational
	}

	opened, err := database.Open(dir, relational)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = opened.Close() })

	return opened
}

// KV reports whether tests run on the database package (STORE=kv).
func KV() bool { return os.Getenv("STORE") == "kv" }

// SQL is the SQLite database under a store (the whole store, or the parts
// the database package has not taken yet), for tests that read its tables.
func SQL(opened store.Store) *sql.DB {
	if wrapped, ok := opened.(*database.Store); ok {
		opened = wrapped.Store
	}

	return opened.(*sqlite.Store).DB
}
