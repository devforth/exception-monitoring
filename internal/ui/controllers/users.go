package controllers

import (
	"fmt"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// The users page (admins): add users, change their name and role, give a
// temporary password, delete them. Whom an admin may change is checked by
// services.Users.

var userNotices = map[string]string{
	"saved":   "Saved.",
	"deleted": "User deleted.",
}

func (controller *Controllers) UsersPage(w http.ResponseWriter, r *http.Request) {
	users, err := controller.users.List(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.views.Render(w, r, http.StatusOK, "users", contracts.UsersData{Users: users, Notice: userNotices[r.URL.Query().Get("notice")]})
}

func (controller *Controllers) NewUserPage(w http.ResponseWriter, r *http.Request) {
	controller.views.Render(w, r, http.StatusOK, "user", contracts.UserForm{New: true, User: store.User{Role: store.RoleUser}})
}

func (controller *Controllers) CreateUser(w http.ResponseWriter, r *http.Request) {
	viewer := requests.Viewer(r)
	input := requests.ReadUser(r)
	id, password, problems, err := controller.users.Create(r.Context(), viewer, input)

	if controller.failed(w, r, err) {
		return
	}

	if len(problems) > 0 {
		view := contracts.UserForm{New: true, User: store.User{Username: input.Username, Name: input.Name, Role: input.Role}, Errors: problems}
		controller.views.Render(w, r, http.StatusUnprocessableEntity, "user", view)

		return
	}

	controller.renderUser(w, r, id, password, "")
}

// UserPage shows a user; the viewer's own account has its own page.
func (controller *Controllers) UserPage(w http.ResponseWriter, r *http.Request) {
	id, ok := controller.pathID(w, r, "userID")

	if !ok {
		return
	}

	if id == requests.Viewer(r).ID {
		http.Redirect(w, r, AccountPath, http.StatusSeeOther)

		return
	}

	controller.renderUser(w, r, id, "", userNotices[r.URL.Query().Get("notice")])
}

func (controller *Controllers) UpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := controller.pathID(w, r, "userID")

	if !ok {
		return
	}

	viewer := requests.Viewer(r)
	input := requests.ReadUser(r)
	problems, err := controller.users.Update(r.Context(), viewer, id, input)

	if controller.failed(w, r, err) {
		return
	}

	if len(problems) > 0 {
		user, err := controller.users.Get(r.Context(), id)

		if controller.failed(w, r, err) {
			return
		}

		user.Name, user.Role = input.Name, input.Role
		controller.views.Render(w, r, http.StatusUnprocessableEntity, "user", contracts.UserForm{User: user, Errors: problems})

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/users/%d?notice=saved", id))
}

// ResetUserPassword gives the user a temporary password, shown this once.
func (controller *Controllers) ResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := controller.pathID(w, r, "userID")

	if !ok {
		return
	}

	password, err := controller.users.ResetPassword(r.Context(), requests.Viewer(r), id)

	if controller.failed(w, r, err) {
		return
	}

	controller.renderUser(w, r, id, password, "")
}

func (controller *Controllers) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := controller.pathID(w, r, "userID")

	if !ok {
		return
	}

	if controller.failed(w, r, controller.users.Delete(r.Context(), requests.Viewer(r), id)) {
		return
	}

	redirectAfterPost(w, r, "/users?notice=deleted")
}

// renderUser shows a user's page, with a temporary password just set.
func (controller *Controllers) renderUser(w http.ResponseWriter, r *http.Request, id int64, password, notice string) {
	user, err := controller.users.Get(r.Context(), id)

	if controller.failed(w, r, err) {
		return
	}

	controller.views.Render(w, r, http.StatusOK, "user", contracts.UserForm{User: user, TemporaryPassword: password, Notice: notice})
}
