package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// Roles of the panel's users. What each may do is decided by the methods of
// User below, the one place for these rules: routes, services and templates
// all ask them.
//
//	superadmin: everything; one account, created from ADMIN_USERNAME/ADMIN_PASSWORD
//	admin:      projects, notifications, users (but not the superadmin), deleting issues
//	user:       issues (status only), logs, projects read-only, their own MCP agents
const (
	RoleSuperadmin = "superadmin"
	RoleAdmin      = "admin"
	RoleUser       = "user"
)

// RoleSummaries say what each role may do, for the user form; they follow
// the methods below.
var RoleSummaries = map[string]string{
	RoleSuperadmin: "everything",
	RoleAdmin:      "projects, notifications, users and deleting issues",
	RoleUser:       "issue statuses, logs and their own coding agents",
}

var ErrUsernameTaken = errors.New("username is taken")

// User is a user of the panel. The password hash is never read with it: only
// UserPassword and FindLogin return it, where a password is checked.
type User struct {
	ID                 int64
	Username           string
	Name               string
	Role               string
	MustChangePassword bool
	CreatedAt          time.Time
	LastLoginAt        *time.Time
}

// DisplayName is the name, or the username when no name is set.
func (user User) DisplayName() string {
	return displayName(user.Name, user.Username)
}

func displayName(name, username string) string {
	if name != "" {
		return name
	}

	return username
}

// CanAdminister: projects, notifications, users and deleting issues.
func (user User) CanAdminister() bool {
	return user.Role == RoleSuperadmin || user.Role == RoleAdmin
}

// CanManage reports whether user may change target and what is theirs
// (edit, reset the password, delete, disconnect their agents): never the
// superadmin, and not themselves here (they have the account page).
func (user User) CanManage(target User) bool {
	return user.CanAdminister() && target.ID != user.ID && target.Role != RoleSuperadmin
}

// CanAccessAgentsOf: one's own MCP agents, or those of a user one manages.
func (user User) CanAccessAgentsOf(owner User) bool {
	return owner.ID == user.ID || user.CanManage(owner)
}

// AssignableRoles are the roles user may give: there is one superadmin, so
// never that one.
func (user User) AssignableRoles() []string {
	if !user.CanAdminister() {
		return nil
	}

	return []string{RoleAdmin, RoleUser}
}

// CanAssign reports whether user may give role.
func (user User) CanAssign(role string) bool {
	return slices.Contains(user.AssignableRoles(), role)
}

// Actor is the user as the author of a change made through source.
func (user User) Actor(source string) Actor {
	return Actor{UserID: user.ID, Name: user.DisplayName(), Source: source}
}

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

// PasswordChange is a new password hash and what it ends.
type PasswordChange struct {
	Hash               string
	MustChangePassword bool
	// KeepSession is the hash of the session that stays logged in (the user
	// changing their own password); every other session of the user ends.
	KeepSession string
	// RevokeAgents disconnects the user's agents: the password was changed
	// by someone else, so whoever had the account may have connected them.
	RevokeAgents bool
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
