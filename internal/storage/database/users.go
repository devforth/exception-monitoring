package database

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// userRecord is a user with the password hash, which only FindLogin and UserPassword return.
type userRecord struct {
	model.User
	PasswordHash string `json:"password_hash"`
}

// sessionRecord is a login to the UI; the cookie itself is never stored.
type sessionRecord struct {
	TokenHash string    `json:"token_hash"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// freeID returns a new random ID that no record of collection has.
func freeID[T any](reader kv.Reader, collection *kv.Collection[T]) (string, error) {
	for {
		id := model.NewID()

		if _, err := collection.Get(reader, kv.K(kv.Text(id))); errors.Is(err, kv.ErrNotFound) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
}

func (database *Store) user(reader kv.Reader, id string) (userRecord, error) {
	user, err := database.users.Get(reader, kv.K(kv.Text(id)))

	return user, notFound(err)
}

// CreateUser stores a new user; ErrUsernameTaken when the username is taken in any case.
func (database *Store) CreateUser(_ context.Context, user model.User, passwordHash string) (string, error) {
	err := database.db.Update(true, func(batch *kv.Batch) error {
		var err error

		if user.ID, err = freeID(batch, database.users); err != nil {
			return err
		}

		return database.users.Insert(batch, userRecord{User: user, PasswordHash: passwordHash})
	})

	if conflict := (kv.ErrConflict{}); errors.As(err, &conflict) && conflict.Index == "name" {
		return "", model.ErrUsernameTaken
	}

	return user.ID, err
}

func (database *Store) GetUser(_ context.Context, id string) (model.User, error) {
	user, err := database.user(database.db, id)

	return user.User, err
}

// FindUser finds a user by username, in any case.
func (database *Store) FindUser(_ context.Context, username string) (model.User, error) {
	user, err := database.users.GetBy(database.db, "name", kv.K(kv.Text(strings.ToLower(username))))

	return user.User, notFound(err)
}

// FindLogin finds a user by username with their password hash, for a login.
func (database *Store) FindLogin(_ context.Context, username string) (model.User, string, error) {
	user, err := database.users.GetBy(database.db, "name", kv.K(kv.Text(strings.ToLower(username))))

	return user.User, user.PasswordHash, notFound(err)
}

// UserPassword returns a user's password hash, to check a password.
func (database *Store) UserPassword(_ context.Context, id string) (string, error) {
	user, err := database.user(database.db, id)

	return user.PasswordHash, err
}

// Superadmin returns the superadmin; ErrNotFound when there is none yet.
func (database *Store) Superadmin(context.Context) (model.User, error) {
	user, err := database.users.GetBy(database.db, "superadmin", kv.K(kv.U8(0)))

	return user.User, notFound(err)
}

// ListUsers lists the users by role (superadmin first), then username.
func (database *Store) ListUsers(context.Context) ([]model.User, error) {
	records, err := database.users.Primary().All(database.db)

	if err != nil {
		return nil, err
	}

	rank := map[string]int{model.RoleSuperadmin: 0, model.RoleAdmin: 1}
	users := make([]model.User, 0, len(records))

	for _, record := range records {
		users = append(users, record.User)
	}

	slices.SortFunc(users, func(a, b model.User) int {
		return cmp.Or(cmp.Compare(cmp.Or(rank[a.Role], 2), cmp.Or(rank[b.Role], 2)), strings.Compare(strings.ToLower(a.Username), strings.ToLower(b.Username)))
	})

	return users, nil
}

// changeUser rewrites a user in one batch; change may also write other records.
func (database *Store) changeUser(id string, change func(batch *kv.Batch, user *userRecord) error) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		user, err := database.user(batch, id)

		if err != nil {
			return err
		}

		if err := change(batch, &user); err != nil {
			return err
		}

		return database.users.Save(batch, user)
	})
}

// UpdateUser changes a user's name and role.
func (database *Store) UpdateUser(_ context.Context, id string, name, role string) error {
	return database.changeUser(id, func(_ *kv.Batch, user *userRecord) error {
		user.Name, user.Role = name, role

		return nil
	})
}

// UpdateUserName changes only the name, so a role changed meanwhile stays.
func (database *Store) UpdateUserName(_ context.Context, id string, name string) error {
	return database.changeUser(id, func(_ *kv.Batch, user *userRecord) error {
		user.Name = name

		return nil
	})
}

// SetPassword replaces a user's password, ends their other sessions and, if asked, disconnects their agents.
func (database *Store) SetPassword(_ context.Context, id string, change model.PasswordChange) error {
	return database.changeUser(id, func(batch *kv.Batch, user *userRecord) error {
		user.PasswordHash, user.MustChangePassword = change.Hash, change.MustChangePassword

		err := database.deleteSessionsOf(batch, id, change.KeepSession)

		if err == nil && change.RevokeAgents {
			err = database.deleteAgentsOf(batch, id)
		}

		return err
	})
}

// DeleteUser removes a user with their sessions and agents; the status changes they made keep their name.
func (database *Store) DeleteUser(_ context.Context, id string) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		user, err := database.user(batch, id)

		if err != nil {
			return err
		}

		if err := database.users.Delete(batch, user); err != nil {
			return err
		}

		if err := database.deleteSessionsOf(batch, id, ""); err != nil {
			return err
		}

		return database.deleteAgentsOf(batch, id)
	})
}

// RecordLogin notes the time of a user's login.
func (database *Store) RecordLogin(_ context.Context, id string, now time.Time) error {
	return database.changeUser(id, func(_ *kv.Batch, user *userRecord) error {
		at := now.UTC()
		user.LastLoginAt = &at

		return nil
	})
}

// CreateSession stores a login, first dropping expired sessions (login is rare).
func (database *Store) CreateSession(_ context.Context, tokenHash string, userID string, now, expiresAt time.Time) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		err := database.sessions.Primary().Where(func(session sessionRecord) bool { return !session.ExpiresAt.After(now) }).
			Each(batch, func(entry kv.Entry[sessionRecord]) (bool, error) {
				database.sessions.DeleteEntry(batch, entry)

				return true, nil
			})

		if err != nil {
			return err
		}

		return database.sessions.Insert(batch, sessionRecord{TokenHash: tokenHash, UserID: userID, CreatedAt: now.UTC(), ExpiresAt: expiresAt.UTC()})
	})
}

// SessionUser returns the user of a session that has not expired; ErrNotFound otherwise.
func (database *Store) SessionUser(_ context.Context, tokenHash string, now time.Time) (model.User, error) {
	session, err := database.sessions.Get(database.db, kv.K(kv.Text(tokenHash)))

	if err != nil {
		return model.User{}, notFound(err)
	}

	if !session.ExpiresAt.After(now) {
		return model.User{}, model.ErrNotFound
	}

	user, err := database.user(database.db, session.UserID)

	return user.User, err
}

func (database *Store) DeleteSession(_ context.Context, tokenHash string) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		_, err := database.sessions.DeleteKey(batch, kv.K(kv.Text(tokenHash)))

		return err
	})
}

// deleteSessionsOf ends a user's sessions but keep.
func (database *Store) deleteSessionsOf(batch *kv.Batch, userID string, keep string) error {
	return database.sessions.By("user", kv.Text(userID)).Each(batch, func(entry kv.Entry[sessionRecord]) (bool, error) {
		if entry.Record.TokenHash != keep {
			database.sessions.DeleteEntry(batch, entry)
		}

		return true, nil
	})
}
