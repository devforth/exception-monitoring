package controllers

import (
	"net/http"
	"net/url"

	"github.com/devforth/exception-monitoring/internal/api/mcp"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/ui/presenters"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// The MCP page: how to connect each agent, the personal token and the
// connected agents.

func (controller *Controllers) AgentsPage(w http.ResponseWriter, r *http.Request) {
	controller.renderAgents(w, r, r.URL.Query().Get("client"), "")
}

// renderAgents shows the page; token is a personal token just created,
// shown this once (only its hash is stored). Admins may list the agents of
// a user they manage (?user=).
func (controller *Controllers) renderAgents(w http.ResponseWriter, r *http.Request, client, token string) {
	page, err := controller.agents.Page(r.Context(), requests.Viewer(r), r.URL.Query().Get("user"))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	view := presenters.Agents(presenters.AgentsInput{
		Endpoint: controller.projects.URL(r, mcp.Path), ServerName: config.AppSlug(controller.appName), Client: client, Token: token,
		IPsRestricted: controller.ipsRestricted, Unencrypted: controller.agents.Unencrypted(r),
		Owner: page.Owner, Owners: page.Owners, Grants: page.Grants, TokenSet: page.TokenSet,
	})

	if requests.IsHTMX(r) {
		controller.views.RenderFragment(w, r, "agents", "mcp-settings", view)

		return
	}

	controller.views.Render(w, r, http.StatusOK, "agents", view)
}

// NewAgentToken replaces the viewer's personal token and shows the new one.
func (controller *Controllers) NewAgentToken(w http.ResponseWriter, r *http.Request) {
	token, err := controller.agents.NewPersonalToken(r.Context(), requests.Viewer(r))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.renderAgents(w, r, "other", token)
}

// DisconnectAgent revokes a connected agent or a personal token: the
// viewer's own, or of a user they manage.
func (controller *Controllers) DisconnectAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := controller.pathID(w, r, "grantID")

	if !ok {
		return
	}

	if controller.failed(w, r, controller.agents.Disconnect(r.Context(), requests.Viewer(r), id)) {
		return
	}

	target := url.Values{"client": {presenters.AgentClient(r.PostFormValue("client"))}}

	if owner := r.PostFormValue("user"); owner != "" {
		target.Set("user", owner)
	}

	redirectAfterPost(w, r, "/agents?"+target.Encode())
}
