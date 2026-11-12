package store

import (
	"context"
	"time"
)

// Sessions are tied to a fingerprint of the admin credentials (computed by the
// UI with a slow key derivation, so it does not reveal the password). A session
// is valid only while the credentials are the same as when it was created.

// CreateSession stores a login session. tokenHash is the hash of the cookie
// value; the cookie value itself is never stored.
func (store *Store) CreateSession(ctx context.Context, tokenHash, credentials string, now, expiresAt time.Time) error {
	// Login is rare, so it is a good moment to drop expired sessions.
	_, err := store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, formatTime(now))

	if err != nil {
		return err
	}

	_, err = store.DB.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, credentials, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, credentials, formatTime(now), formatTime(expiresAt))

	return err
}

// SessionValid reports whether a session with this token hash exists, has not
// expired and was created with the current credentials.
func (store *Store) SessionValid(ctx context.Context, tokenHash, credentials string, now time.Time) (bool, error) {
	var count int

	err := store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE token_hash = ? AND credentials = ? AND expires_at > ?`,
		tokenHash, credentials, formatTime(now)).Scan(&count)

	return count > 0, err
}

// DeleteSessionsExcept removes sessions created with other credentials. Called
// at startup, so old sessions do not linger after the password changes.
func (store *Store) DeleteSessionsExcept(ctx context.Context, credentials string) error {
	_, err := store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE credentials != ?`, credentials)

	return err
}

func (store *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)

	return err
}
