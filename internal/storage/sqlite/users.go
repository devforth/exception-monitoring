package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const userFields = `id, username, name, role, must_change_password, created_at, last_login_at`

func scanUser(row scanner) (User, error) {
	var (
		user      User
		createdAt string
		lastLogin sql.NullString
	)

	err := row.Scan(&user.ID, &user.Username, &user.Name, &user.Role, &user.MustChangePassword, &createdAt, &lastLogin)

	if errors.Is(err, sql.ErrNoRows) {
		return user, ErrNotFound
	}

	user.CreatedAt, user.LastLoginAt = parseTime(createdAt), parseOptionalTime(lastLogin)

	return user, err
}

// CreateUser stores a new user with a password hash; ErrUsernameTaken when
// the username (in any case) exists.
func (store *Store) CreateUser(ctx context.Context, user User, passwordHash string) (int64, error) {
	result, err := store.DB.ExecContext(ctx, `INSERT INTO users (username, name, role, password_hash, must_change_password, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		user.Username, user.Name, user.Role, passwordHash, user.MustChangePassword, formatTime(user.CreatedAt))

	if isUniqueViolation(err) {
		return 0, ErrUsernameTaken
	}

	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

func (store *Store) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(store.DB.QueryRowContext(ctx, `SELECT `+userFields+` FROM users WHERE id = ?`, id))
}

// FindUser finds a user by username, in any case.
func (store *Store) FindUser(ctx context.Context, username string) (User, error) {
	return scanUser(store.DB.QueryRowContext(ctx, `SELECT `+userFields+` FROM users WHERE username = ?`, username))
}

// FindLogin finds a user by username with their password hash, for a login.
func (store *Store) FindLogin(ctx context.Context, username string) (User, string, error) {
	user, err := store.FindUser(ctx, username)

	if err != nil {
		return user, "", err
	}

	hash, err := store.UserPassword(ctx, user.ID)

	return user, hash, err
}

// UserPassword returns a user's password hash, to check a password.
func (store *Store) UserPassword(ctx context.Context, id int64) (string, error) {
	var hash string

	err := store.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash)

	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}

	return hash, err
}

// Superadmin returns the superadmin; ErrNotFound when there is none yet.
func (store *Store) Superadmin(ctx context.Context) (User, error) {
	// The role in the SQL, not a parameter: then the partial index applies.
	return scanUser(store.DB.QueryRowContext(ctx, `SELECT `+userFields+` FROM users WHERE role = 'superadmin'`))
}

// ListUsers lists the users by role (superadmin first), then username.
func (store *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := store.DB.QueryContext(ctx, `SELECT `+userFields+` FROM users
		ORDER BY CASE role WHEN ? THEN 0 WHEN ? THEN 1 ELSE 2 END, username COLLATE NOCASE`, RoleSuperadmin, RoleAdmin)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var users []User

	for rows.Next() {
		user, err := scanUser(rows)

		if err != nil {
			return nil, err
		}

		users = append(users, user)
	}

	return users, rows.Err()
}

// UpdateUser changes a user's name and role.
func (store *Store) UpdateUser(ctx context.Context, id int64, name, role string) error {
	return affectedOne(store.DB.ExecContext(ctx, `UPDATE users SET name = ?, role = ? WHERE id = ?`, name, role, id))
}

// UpdateUserName changes only the name, so a role changed meanwhile stays.
func (store *Store) UpdateUserName(ctx context.Context, id int64, name string) error {
	return affectedOne(store.DB.ExecContext(ctx, `UPDATE users SET name = ? WHERE id = ?`, name, id))
}

// SetPassword replaces a user's password and ends their other sessions.
func (store *Store) SetPassword(ctx context.Context, id int64, change PasswordChange) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	err = affectedOne(transaction.ExecContext(ctx, `UPDATE users SET password_hash = ?, must_change_password = ? WHERE id = ?`,
		change.Hash, change.MustChangePassword, id))

	if err != nil {
		return err
	}

	if _, err := transaction.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, id, change.KeepSession); err != nil {
		return err
	}

	if change.RevokeAgents {
		for _, query := range []string{`DELETE FROM oauth_codes WHERE user_id = ?`, `DELETE FROM agent_grants WHERE user_id = ?`} {
			if _, err := transaction.ExecContext(ctx, query, id); err != nil {
				return err
			}
		}
	}

	return transaction.Commit()
}

// DeleteUser removes a user with their sessions and agents (foreign keys);
// the status changes they made keep their name.
func (store *Store) DeleteUser(ctx context.Context, id int64) error {
	return affectedOne(store.DB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id))
}

// RecordLogin notes the time of a user's login.
func (store *Store) RecordLogin(ctx context.Context, id int64, now time.Time) error {
	_, err := store.DB.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, formatTime(now), id)

	return err
}

// AdoptAgents gives agents connected before there were users to userID (the
// superadmin), so they keep working.
func (store *Store) AdoptAgents(ctx context.Context, userID int64) error {
	if _, err := store.DB.ExecContext(ctx, `DELETE FROM oauth_codes WHERE user_id IS NULL`); err != nil {
		return err
	}

	_, err := store.DB.ExecContext(ctx, `UPDATE agent_grants SET user_id = ? WHERE user_id IS NULL`, userID)

	return err
}
