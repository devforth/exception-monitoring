// Package store owns the SQLite database: opening it, tuning it and applying migrations.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

type Store struct {
	DB *sql.DB

	// logSearchReady: every log is in the search index (log_search.go).
	logSearchReady atomic.Bool
}

// Open opens (or creates) the database file at path, with its directory,
// and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	return openWithMigrations(ctx, path, migrations)
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
		"&_pragma=foreign_keys(ON)" +
		// After a burst of writes the WAL file is cut back to 64 MB instead of
		// keeping its largest size.
		"&_pragma=journal_size_limit(67108864)"

	db, err := sql.Open("sqlite", connectionString)

	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // the open error is the one to report
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	if err := enableIncrementalVacuum(ctx, db); err != nil {
		_ = db.Close() // the open error is the one to report
		return nil, err
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

// enableIncrementalVacuum lets retention give deleted space back to the file
// system (see ReleaseFreeSpace); without it the file never shrinks. The mode
// of an existing database changes only with a full VACUUM, done once.
func enableIncrementalVacuum(ctx context.Context, db *sql.DB) error {
	const incremental = 2

	var mode int

	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return fmt.Errorf("read auto_vacuum: %w", err)
	}

	if mode == incremental {
		return nil
	}

	// One connection: the pragma applies to it, and VACUUM runs outside a transaction.
	connection, err := db.Conn(ctx)

	if err != nil {
		return err
	}

	defer func() { _ = connection.Close() }() // returns it to the pool; nothing to report

	if _, err := connection.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return fmt.Errorf("set auto_vacuum: %w", err)
	}

	if _, err := connection.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum to enable auto_vacuum: %w", err)
	}

	return nil
}
