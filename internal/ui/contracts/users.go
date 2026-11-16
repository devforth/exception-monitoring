package contracts

import "github.com/devforth/exception-monitoring/internal/store"

// The logged-in user is a store.User (requests.Viewer, $.Viewer in
// templates); what their role allows is asked from its methods.

// UsersData is the users page.
type UsersData struct {
	Users  []store.User
	Notice string
}

// UserForm is the page of a new or an existing user.
type UserForm struct {
	New  bool
	User store.User // as typed for a new user
	// TemporaryPassword was just set (a new user or a reset): shown once,
	// to pass on to the user, who must change it at the next login.
	TemporaryPassword string
	Notice            string
	Errors            map[string]string
}

// UserInput is what the user form sends.
type UserInput struct {
	Username string
	Name     string
	Role     string
}

// AccountData is the page where users change their own name and password
// (who they are is $.Viewer).
type AccountData struct {
	Notice string
	Errors map[string]string
}
