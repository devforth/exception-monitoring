package store

import (
	"context"
	"database/sql"
	"fmt"
)

type migration struct {
	version int
	sql     string
}

// migrations are applied in order, each once. Never edit an applied migration; add a new one.
var migrations = []migration{}

func migrate(ctx context.Context, db *sql.DB, migrationList []migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var currentVersion int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&currentVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if currentVersion > len(migrationList) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); run a newer version", currentVersion, len(migrationList))
	}

	for index, step := range migrationList {
		if step.version != index+1 {
			return fmt.Errorf("migration %d has version %d, versions must be sequential from 1", index+1, step.version)
		}
		if step.version <= currentVersion {
			continue
		}
		if err := apply(ctx, db, step); err != nil {
			return fmt.Errorf("migration %d: %w", step.version, err)
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, step migration) error {
	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()

	if _, err := transaction.ExecContext(ctx, step.sql); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, step.version); err != nil {
		return err
	}
	return transaction.Commit()
}
