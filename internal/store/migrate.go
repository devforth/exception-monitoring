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
//
// Times are TEXT in UTC with a fixed width (see timeFormat), so they sort
// correctly as strings and stay readable in the sqlite3 shell.
// project_id has no foreign key: projects are defined in env, not in the database.
var migrations = []migration{
	{1, `
CREATE TABLE issues (
	id          INTEGER PRIMARY KEY,
	project_id  INTEGER NOT NULL,
	fingerprint TEXT    NOT NULL,
	title       TEXT    NOT NULL,
	culprit     TEXT    NOT NULL DEFAULT '',
	level       TEXT    NOT NULL,
	status      TEXT    NOT NULL DEFAULT 'unresolved' CHECK (status IN ('unresolved', 'resolved', 'ignored')),
	event_count INTEGER NOT NULL DEFAULT 0,
	first_seen  TEXT    NOT NULL,
	last_seen   TEXT    NOT NULL,
	resolved_at TEXT,
	UNIQUE (project_id, fingerprint)
);
CREATE INDEX issues_by_project_last_seen ON issues (project_id, last_seen DESC);

CREATE TABLE events (
	id          INTEGER PRIMARY KEY,
	project_id  INTEGER NOT NULL,
	issue_id    INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
	event_id    TEXT    NOT NULL,
	timestamp   TEXT    NOT NULL,
	received_at TEXT    NOT NULL,
	level       TEXT    NOT NULL,
	environment TEXT    NOT NULL DEFAULT '',
	release     TEXT    NOT NULL DEFAULT '',
	trace_id    TEXT    NOT NULL DEFAULT '',
	payload     TEXT    NOT NULL,
	UNIQUE (project_id, event_id)
);
CREATE INDEX events_by_issue_time ON events (issue_id, received_at DESC);
CREATE INDEX events_by_trace ON events (trace_id) WHERE trace_id != '';

CREATE TABLE logs (
	id              INTEGER PRIMARY KEY,
	project_id      INTEGER NOT NULL,
	timestamp       TEXT    NOT NULL,
	level           TEXT    NOT NULL,
	severity_number INTEGER NOT NULL DEFAULT 0,
	body            TEXT    NOT NULL,
	trace_id        TEXT    NOT NULL DEFAULT '',
	attributes      TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX logs_by_project_time ON logs (project_id, timestamp DESC);
CREATE INDEX logs_by_trace ON logs (trace_id) WHERE trace_id != '';
`},
}

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
