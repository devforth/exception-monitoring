package controllers

import (
	"net/http"

	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
	"github.com/devforth/exception-monitoring/internal/ui/session"
)

func (controller *Controllers) LoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := controller.auth.SessionUser(r); ok {
		http.Redirect(w, r, requests.SafeNext(r.URL.Query().Get("next")), http.StatusSeeOther)

		return
	}

	controller.views.Render(w, r, http.StatusOK, "login", contracts.LoginData{Next: requests.SafeNext(r.URL.Query().Get("next")), Disabled: controller.auth.Disabled(r.Context())})
}

func (controller *Controllers) Login(w http.ResponseWriter, r *http.Request) {
	data := contracts.LoginData{Next: requests.SafeNext(r.FormValue("next")), Username: r.FormValue("username"), Disabled: controller.auth.Disabled(r.Context())}

	if data.Disabled {
		controller.views.Render(w, r, http.StatusForbidden, "login", data)

		return
	}

	result, err := controller.auth.Login(w, r, r.FormValue("username"), r.FormValue("password"))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	switch result {
	case session.LoginBlocked:
		data.Error = session.TooManyAttempts
		controller.views.Render(w, r, http.StatusTooManyRequests, "login", data)
	case session.LoginWrongCredentials:
		data.Error = "Wrong username or password."
		controller.views.Render(w, r, http.StatusUnauthorized, "login", data)
	default:
		http.Redirect(w, r, data.Next, http.StatusSeeOther)
	}
}

func (controller *Controllers) Logout(w http.ResponseWriter, r *http.Request) {
	controller.auth.EndSession(r.Context(), w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
