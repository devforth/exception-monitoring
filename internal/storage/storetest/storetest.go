// Package storetest opens a store for tests.
package storetest

import (
	"testing"

	"github.com/devforth/exception-monitoring/internal/storage/database"
)

// Open opens a new store in a temporary directory, closed with the test.
func Open(t testing.TB) *database.Store {
	t.Helper()

	opened, err := database.Open(t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = opened.Close() })

	return opened
}
