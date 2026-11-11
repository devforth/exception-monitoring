// Package store owns the SQLite database: opening it, tuning it and applying migrations.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

const fileName = "exception-monitoring.db"

type Store struct {
	DB *sql.DB
}

// Open opens (or creates) the database in dataDir and applies pending migrations.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	return openWithMigrations(ctx, filepath.Join(dataDir, fileName), migrations)
}

func openWithMigrations(ctx context.Context, path string, migrationList []migration) (*Store, error) {
	// WAL: readers don't block the writer. busy_timeout: wait instead of failing on a locked DB.
	// _txlock=immediate: take the write lock at BEGIN. With the default (deferred), two
	// read-then-write transactions deadlock on lock upgrade and one fails with SQLITE_BUSY
	// immediately, ignoring busy_timeout.
	connectionString := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", connectionString)

	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // the open error is the one to report
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	if err := migrate(ctx, db, migrationList); err != nil {
		_ = db.Close() // the open error is the one to report
		return nil, err
	}

	return &Store{DB: db}, nil
}

func (store *Store) Ping(ctx context.Context) error {
	return store.DB.PingContext(ctx)
}

func (store *Store) Close() error {
	return store.DB.Close()
}
