package store

import (
	"context"
	"time"
)

// Sessions are UI logins of a user. Deleting the user ends them (foreign
// key), and so does a password change (SetPassword).

// CreateSession stores a login session. tokenHash is the hash of the cookie
// value; the cookie value itself is never stored.
func (store *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, now, expiresAt time.Time) error {
	// Login is rare, so it is a good moment to drop expired sessions.
	_, err := store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, formatTime(now))

	if err != nil {
		return err
	}

	_, err = store.DB.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, userID, formatTime(now), formatTime(expiresAt))

	return err
}

// SessionUser returns the user of a session that has not expired;
// ErrNotFound otherwise.
func (store *Store) SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error) {
	return scanUser(store.DB.QueryRowContext(ctx, `SELECT `+userFields+` FROM users
		WHERE id = (SELECT user_id FROM sessions WHERE token_hash = ? AND expires_at > ?)`, tokenHash, formatTime(now)))
}

func (store *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)

	return err
}
