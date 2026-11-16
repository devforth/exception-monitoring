package controllers

import (
	"net/http"

	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
	"github.com/devforth/exception-monitoring/internal/ui/services"
)

// The account page: every user changes their own name and password here,
// and is sent here after logging in with a temporary password.

// AccountPath is the account page.
const AccountPath = "/account"

var accountNotices = map[string]string{
	"name":     "Name saved.",
	"password": "Password changed. Your other sessions were logged out; your connected agents keep working.",
}

func (controller *Controllers) AccountPage(w http.ResponseWriter, r *http.Request) {
	controller.renderAccount(w, r, http.StatusOK, accountNotices[r.URL.Query().Get("notice")], nil)
}

func (controller *Controllers) UpdateAccountName(w http.ResponseWriter, r *http.Request) {
	problems, err := controller.users.UpdateName(r.Context(), requests.Viewer(r), r.PostFormValue("name"))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if len(problems) > 0 {
		controller.renderAccount(w, r, http.StatusUnprocessableEntity, "", problems)

		return
	}

	redirectAfterPost(w, r, AccountPath+"?notice=name")
}

func (controller *Controllers) ChangeAccountPassword(w http.ResponseWriter, r *http.Request) {
	problems, err := controller.auth.ChangePassword(r, requests.Viewer(r),
		r.PostFormValue("current_password"), r.PostFormValue("password"), r.PostFormValue("confirm_password"))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if len(problems) > 0 {
		controller.renderAccount(w, r, http.StatusUnprocessableEntity, "", problems)

		return
	}

	redirectAfterPost(w, r, AccountPath+"?notice=password")
}

func (controller *Controllers) renderAccount(w http.ResponseWriter, r *http.Request, status int, notice string, problems services.Problems) {
	controller.views.Render(w, r, status, "account", contracts.AccountData{Notice: notice, Errors: problems})
}
