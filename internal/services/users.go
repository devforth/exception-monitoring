package services

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// ErrForbidden: the viewer's role does not allow the change.
var ErrForbidden = errors.New("not allowed")

// Users manages the panel's users. Who may change whom is decided by the
// methods of store.User; every method checks it again, whatever the page
// showed. Users change their own password through Auth.
type Users struct {
	logger *slog.Logger
	store  store.Store
	now    func() time.Time
}

func NewUsers(logger *slog.Logger, database store.Store) *Users {
	return &Users{logger: logger, store: database, now: time.Now}
}

var validUsername = regexp.MustCompile(`^` + config.UsernamePattern + `$`)

// Problems are invalid fields with their messages, keyed by field name.
type Problems map[string]string

// EnsureSuperadmin creates the superadmin from ADMIN_USERNAME and
// ADMIN_PASSWORD when there is none: once, at the first start. Later the
// variables are not used.
func (users *Users) EnsureSuperadmin(ctx context.Context, username, password string) error {
	_, err := users.store.Superadmin(ctx)

	switch {
	case err == nil:
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return err
	case password == "":
		users.logger.Warn("no superadmin yet: set ADMIN_PASSWORD (and ADMIN_USERNAME) to create one, the UI is closed until then")

		return nil
	}

	hash, err := HashPassword(password)

	if err != nil {
		return err
	}

	_, err = users.store.CreateUser(ctx, store.User{Username: username, Name: username, Role: store.RoleSuperadmin, CreatedAt: users.now()}, hash)

	if errors.Is(err, store.ErrUsernameTaken) {
		return fmt.Errorf("ADMIN_USERNAME %q belongs to an existing user: choose another username for the superadmin", username)
	}

	if err != nil {
		return err
	}

	users.logger.Info("superadmin created", "username", username)

	return nil
}

func (users *Users) List(ctx context.Context) ([]store.User, error) {
	return users.store.ListUsers(ctx)
}

func (users *Users) Get(ctx context.Context, id string) (store.User, error) {
	return users.store.GetUser(ctx, id)
}

// Create adds a user with a temporary password, returned to be shown once.
func (users *Users) Create(ctx context.Context, viewer store.User, input UserInput) (string, string, Problems, error) {
	if !viewer.CanAdminister() {
		return "", "", nil, ErrForbidden
	}

	input.Username = strings.TrimSpace(input.Username)
	problems := checkUserInput(viewer, input)

	switch {
	case input.Username == "":
		problems["username"] = "Enter a username."
	case utf8.RuneCountInString(input.Username) > config.MaxUsername || !validUsername.MatchString(input.Username):
		problems["username"] = fmt.Sprintf("Up to %d letters, digits and . _ @ -.", config.MaxUsername)
	}

	if len(problems) > 0 {
		return "", "", problems, nil
	}

	password := temporaryPassword()
	hash, err := HashPassword(password)

	if err != nil {
		return "", "", nil, err
	}

	user := store.User{Username: input.Username, Name: nameOr(input.Name, input.Username), Role: input.Role, MustChangePassword: true, CreatedAt: users.now()}
	id, err := users.store.CreateUser(ctx, user, hash)

	if errors.Is(err, store.ErrUsernameTaken) {
		return "", "", Problems{"username": "This username is taken."}, nil
	}

	return id, password, nil, err
}

// Update changes a user's name and role.
func (users *Users) Update(ctx context.Context, viewer store.User, id string, input UserInput) (Problems, error) {
	target, err := users.manageable(ctx, viewer, id)

	if err != nil {
		return nil, err
	}

	if problems := checkUserInput(viewer, input); len(problems) > 0 {
		return problems, nil
	}

	return nil, users.store.UpdateUser(ctx, id, nameOr(input.Name, target.Username), input.Role)
}

// ResetPassword gives a user a temporary password (returned to be shown
// once): their sessions end and their agents are disconnected, since
// whoever knew the old password may have connected them.
func (users *Users) ResetPassword(ctx context.Context, viewer store.User, id string) (string, error) {
	if _, err := users.manageable(ctx, viewer, id); err != nil {
		return "", err
	}

	return users.setTemporaryPassword(ctx, id, true)
}

// Delete removes a user with their sessions and agents; their name stays in
// the issue histories.
func (users *Users) Delete(ctx context.Context, viewer store.User, id string) error {
	if _, err := users.manageable(ctx, viewer, id); err != nil {
		return err
	}

	return users.store.DeleteUser(ctx, id)
}

// UpdateName changes the viewer's own name.
func (users *Users) UpdateName(ctx context.Context, viewer store.User, name string) (Problems, error) {
	problems := Problems{}

	if !checkName(problems, name) {
		return problems, nil
	}

	return nil, users.store.UpdateUserName(ctx, viewer.ID, nameOr(name, viewer.Username))
}

// SetPasswordByOperator is the reset-password command: run with the server
// stopped by whoever runs it, usually the superadmin who forgot the password.
// Sessions end, agents stay; the new password is temporary.
func (users *Users) SetPasswordByOperator(ctx context.Context, username string) (string, error) {
	user, err := users.store.FindUser(ctx, username)

	if err != nil {
		return "", err
	}

	return users.setTemporaryPassword(ctx, user.ID, false)
}

func (users *Users) setTemporaryPassword(ctx context.Context, id string, revokeAgents bool) (string, error) {
	password := temporaryPassword()

	return password, setPassword(ctx, users.store, id, password, store.PasswordChange{MustChangePassword: true, RevokeAgents: revokeAgents})
}

// manageable returns user id when the viewer may change them (ErrForbidden
// otherwise).
func (users *Users) manageable(ctx context.Context, viewer store.User, id string) (store.User, error) {
	target, err := users.store.GetUser(ctx, id)

	if err == nil && !viewer.CanManage(target) {
		err = ErrForbidden
	}

	return target, err
}

// Authenticate checks a login: the user with username and password, ok
// false when either is wrong. An unknown username is checked against a dummy
// hash, so the response time does not tell which usernames exist.
func (users *Users) Authenticate(ctx context.Context, username, password string) (store.User, bool, error) {
	user, hash, err := users.store.FindLogin(ctx, strings.TrimSpace(username))

	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.User{}, false, err
	}

	if !checkPassword(cmp.Or(hash, dummyPasswordHash()), password) || user.ID == "" {
		return store.User{}, false, nil
	}

	return user, true, nil
}

// ChangeOwnPassword replaces the viewer's own password after checking the
// current one; wrongCurrent tells the caller to count a failed attempt. The
// viewer's other sessions end (keepSession is the hash of the one that
// stays); their agents stay connected, since it is the owner who changes it.
func (users *Users) ChangeOwnPassword(ctx context.Context, viewer store.User, current, password, confirm, keepSession string) (problems Problems, wrongCurrent bool, err error) {
	hash, err := users.store.UserPassword(ctx, viewer.ID)

	if err != nil {
		return nil, false, err
	}

	problems = Problems{}

	if !checkPassword(hash, current) {
		wrongCurrent = true
		problems["current_password"] = "The current password is wrong."
	}

	checkNewPassword(problems, current, password, confirm)

	if len(problems) > 0 {
		return problems, wrongCurrent, nil
	}

	return nil, false, setPassword(ctx, users.store, viewer.ID, password, store.PasswordChange{KeepSession: keepSession})
}

// nameOr is the display name as saved: the username when none is given.
func nameOr(name, username string) string {
	return cmp.Or(strings.TrimSpace(name), username)
}

// setPassword stores a new password hash with what the change ends. It
// checks no role: callers are the methods of Users, which do.
func setPassword(ctx context.Context, database store.Store, id string, password string, change store.PasswordChange) error {
	hash, err := HashPassword(password)

	if err != nil {
		return err
	}

	change.Hash = hash

	return database.SetPassword(ctx, id, change)
}

func checkUserInput(viewer store.User, input UserInput) Problems {
	problems := Problems{}
	checkName(problems, input.Name)

	if !viewer.CanAssign(input.Role) {
		problems["role"] = "Choose a role."
	}

	return problems
}

// checkName adds a problem when a display name is too long.
func checkName(problems Problems, name string) bool {
	if utf8.RuneCountInString(strings.TrimSpace(name)) > config.MaxUserName {
		problems["name"] = fmt.Sprintf("At most %d characters.", config.MaxUserName)

		return false
	}

	return true
}

// checkNewPassword adds a problem when a new password is not acceptable.
func checkNewPassword(problems Problems, current, password, confirm string) {
	switch {
	case utf8.RuneCountInString(password) < config.MinPasswordLength:
		problems["password"] = fmt.Sprintf("At least %d characters.", config.MinPasswordLength)
	case len(password) > config.MaxPasswordLength:
		problems["password"] = fmt.Sprintf("At most %d characters.", config.MaxPasswordLength)
	case password == current:
		problems["password"] = "Choose a password different from the current one."
	case password != confirm:
		problems["confirm_password"] = "The passwords do not match."
	}
}
