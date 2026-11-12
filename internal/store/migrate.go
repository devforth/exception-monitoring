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
	{2, `
-- UI login sessions. Only a SHA-256 hash of the cookie token is stored, so a
-- copy of the database file cannot be used to log in.
CREATE TABLE sessions (
	token_hash TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE INDEX sessions_by_expiry ON sessions (expires_at);
`},
	{3, `
-- Ties each session to the admin credentials it was created with, so changing
-- ADMIN_USERNAME or ADMIN_PASSWORD logs everyone out. Sessions created before
-- this column existed get '' and stop working.
ALTER TABLE sessions ADD COLUMN credentials TEXT NOT NULL DEFAULT '';
`},
	{4, `
-- Spans tie logs to the request an error happened in. The trace alone is not
-- enough: a browser keeps one trace for the whole page session, so every
-- request from an open tab shares it. Existing rows are filled from the JSON.
ALTER TABLE events ADD COLUMN span_id TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN parent_span_id TEXT NOT NULL DEFAULT '';
ALTER TABLE logs ADD COLUMN span_id TEXT NOT NULL DEFAULT '';

UPDATE events SET
	span_id = COALESCE(json_extract(payload, '$.contexts.trace.span_id'), ''),
	parent_span_id = COALESCE(json_extract(payload, '$.contexts.trace.parent_span_id'), '');
UPDATE logs SET span_id = COALESCE(json_extract(attributes, '$."sentry.trace.parent_span_id".value'), '');

CREATE INDEX events_by_span ON events (span_id) WHERE span_id != '';
CREATE INDEX events_by_parent_span ON events (parent_span_id) WHERE parent_span_id != '';
CREATE INDEX logs_by_span ON logs (span_id) WHERE span_id != '';
`},
	{5, `
-- Outgoing notifications, one row per message and channel, so they survive a
-- restart and are retried while a chat or mail server is down. Sent rows are
-- deleted after NOTIFY_KEEP_SENT, failed ones after NOTIFY_KEEP_FAILED.
CREATE TABLE notification_outbox (
	id              INTEGER PRIMARY KEY,
	channel         TEXT    NOT NULL,
	kind            TEXT    NOT NULL CHECK (kind IN ('notification', 'summary')),
	payload         TEXT    NOT NULL,
	event_payload   TEXT    NOT NULL DEFAULT '',
	status          TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
	attempts        INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT    NOT NULL DEFAULT '',
	created_at      TEXT    NOT NULL,
	next_attempt_at TEXT    NOT NULL,
	updated_at      TEXT    NOT NULL
);
CREATE INDEX notification_outbox_due ON notification_outbox (channel, status, next_attempt_at);
CREATE INDEX notification_outbox_by_age ON notification_outbox (status, updated_at);
`},
	{6, `
-- Where notifications go, managed in the UI. The outbox "channel" column now
-- holds the connection id.
CREATE TABLE notification_connections (
	id                INTEGER PRIMARY KEY,
	name              TEXT    NOT NULL,
	type              TEXT    NOT NULL CHECK (type IN ('email', 'telegram', 'slack')),
	address           TEXT    NOT NULL DEFAULT '', -- email recipient
	secret            TEXT    NOT NULL DEFAULT '', -- encrypted tokens of telegram / slack
	all_projects      INTEGER NOT NULL DEFAULT 0,  -- every project, including ones added later
	levels            TEXT    NOT NULL,            -- comma-separated event levels
	active            INTEGER NOT NULL DEFAULT 0,
	verified          INTEGER NOT NULL DEFAULT 0,  -- email: the code was confirmed
	verify_code_hash  TEXT    NOT NULL DEFAULT '',
	verify_expires_at TEXT    NOT NULL DEFAULT '',
	verify_attempts   INTEGER NOT NULL DEFAULT 0,
	created_at        TEXT    NOT NULL,
	updated_at        TEXT    NOT NULL,
	CHECK (active = 0 OR verified = 1)
);

CREATE TABLE notification_connection_projects (
	connection_id INTEGER NOT NULL REFERENCES notification_connections (id) ON DELETE CASCADE,
	project_id    INTEGER NOT NULL,
	PRIMARY KEY (connection_id, project_id)
);

-- Messages queued for the channels configured in env before; those channels are gone.
UPDATE notification_outbox SET status = 'failed', last_error = 'channel settings moved to the UI'
WHERE status = 'pending';
`},
	{7, `
-- Destinations (an email address, a Telegram chat, a Slack webhook) move out of
-- connections into their own table. An email address keeps its confirmation
-- when a connection is deleted or a form is left unfinished, so adding it
-- again needs no new code.
CREATE TABLE notification_destinations (
	id                INTEGER PRIMARY KEY,
	type              TEXT    NOT NULL CHECK (type IN ('email', 'telegram', 'slack')),
	address           TEXT    NOT NULL DEFAULT '', -- email, lowercase
	secret            TEXT    NOT NULL DEFAULT '', -- encrypted tokens of telegram / slack
	verified          INTEGER NOT NULL DEFAULT 0,
	verified_at       TEXT    NOT NULL DEFAULT '',
	verify_code_hash  TEXT    NOT NULL DEFAULT '',
	verify_expires_at TEXT    NOT NULL DEFAULT '',
	verify_attempts   INTEGER NOT NULL DEFAULT 0,
	verify_sent_at    TEXT    NOT NULL DEFAULT '',
	created_at        TEXT    NOT NULL,
	updated_at        TEXT    NOT NULL
);
CREATE INDEX notification_destinations_by_address ON notification_destinations (address) WHERE type = 'email';

-- Every existing connection becomes a destination with the same id.
INSERT INTO notification_destinations (id, type, address, secret, verified, verified_at, created_at, updated_at)
SELECT id, type, lower(address), secret, verified, CASE WHEN verified = 1 THEN updated_at ELSE '' END, created_at, updated_at
FROM notification_connections;

-- Rebuild connections without the destination columns. The project rows are
-- copied aside first: dropping the connections table would cascade to them.
CREATE TABLE old_connection_projects AS SELECT * FROM notification_connection_projects;
CREATE TABLE old_connections AS SELECT * FROM notification_connections;
DROP TABLE notification_connection_projects;
DROP TABLE notification_connections;

CREATE TABLE notification_connections (
	id             INTEGER PRIMARY KEY,
	name           TEXT    NOT NULL,
	destination_id INTEGER NOT NULL REFERENCES notification_destinations (id),
	all_projects   INTEGER NOT NULL DEFAULT 0,
	levels         TEXT    NOT NULL,
	active         INTEGER NOT NULL DEFAULT 0,
	created_at     TEXT    NOT NULL,
	updated_at     TEXT    NOT NULL
);

CREATE TABLE notification_connection_projects (
	connection_id INTEGER NOT NULL REFERENCES notification_connections (id) ON DELETE CASCADE,
	project_id    INTEGER NOT NULL,
	PRIMARY KEY (connection_id, project_id)
);

INSERT INTO notification_connections (id, name, destination_id, all_projects, levels, active, created_at, updated_at)
SELECT id, name, id, all_projects, levels, active, created_at, updated_at FROM old_connections;
INSERT INTO notification_connection_projects SELECT * FROM old_connection_projects;
DROP TABLE old_connections;
DROP TABLE old_connection_projects;
`},
	{8, `ALTER TABLE notification_connections ADD COLUMN hide_issue_link INTEGER NOT NULL DEFAULT 0;`},
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
