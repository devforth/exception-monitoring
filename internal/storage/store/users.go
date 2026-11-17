package store

import (
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

// displayName is the name to show: the name, or the username without one.
func displayName(name, username string) string {
	if name != "" {
		return name
	}

	return username
}
