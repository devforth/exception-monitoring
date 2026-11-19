package controllers

import (
	"errors"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/api/oauth"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/middleware"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// AuthorizePage shows the OAuth consent page: the logged-in user allows or
// denies an agent's request.
func (controller *Controllers) AuthorizePage(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	request, err := controller.agents.CheckRequest(r.Context(), r, params)

	if err != nil {
		controller.authorizationError(w, r, err)

		return
	}

	allowAuthorizationRedirect(w, request)
	controller.views.Render(w, r, http.StatusOK, "authorize", contracts.AuthorizeView{
		ClientName: request.ClientName, ClientSite: request.ClientSite(), RedirectURI: request.RedirectURI, Params: params,
		CSRFToken: controller.auth.FormToken(r, oauth.AuthorizePath+"?"+params.Encode()),
	})
}

func (controller *Controllers) Authorize(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid authorization form", http.StatusBadRequest)

		return
	}

	params := r.PostForm
	token, decision := params.Get("csrf_token"), params.Get("decision")
	params.Del("csrf_token")
	params.Del("decision")

	if !controller.auth.ValidFormToken(r, oauth.AuthorizePath+"?"+params.Encode(), token) {
		http.Error(w, "invalid authorization form token", http.StatusForbidden)

		return
	}

	if decision != "allow" && decision != "deny" {
		http.Error(w, "choose Allow or Deny", http.StatusBadRequest)

		return
	}

	target, err := controller.agents.Decide(r.Context(), r, requests.Viewer(r), params, decision == "allow")

	if err != nil {
		controller.authorizationError(w, r, err)

		return
	}

	allowAuthorizationRedirect(w, oauth.Request{RedirectURI: target})
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// allowAuthorizationRedirect lets the consent form send the browser back to
// the agent, which CSP form-action would block (it covers redirects too).
// The target was checked against the agent's registered redirect URIs.
func allowAuthorizationRedirect(w http.ResponseWriter, request oauth.Request) {
	w.Header().Set("Content-Security-Policy", middleware.ContentSecurityPolicy(request.RedirectOrigin()))
}

func (controller *Controllers) authorizationError(w http.ResponseWriter, r *http.Request, err error) {
	var oauthErr *oauth.Error

	if !errors.As(err, &oauthErr) {
		controller.serverError(w, r, err)

		return
	}

	controller.views.Render(w, r, http.StatusBadRequest, "authorize", contracts.AuthorizeView{Error: oauthErr.Description})
}
