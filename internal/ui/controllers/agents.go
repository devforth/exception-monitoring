package controllers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/mcp"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/presenters"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// The MCP page: how to connect each agent, the personal token and the
// connected agents.

func (controller *Controllers) AgentsPage(w http.ResponseWriter, r *http.Request) {
	controller.renderAgents(w, r, r.URL.Query().Get("client"), "")
}

// renderAgents shows the page; token is a personal token just created,
// shown this once (only its hash is stored).
func (controller *Controllers) renderAgents(w http.ResponseWriter, r *http.Request, client, token string) {
	grants, err := controller.agents.Grants(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	view := presenters.Agents(presenters.AgentsInput{
		Endpoint: controller.projects.URL(r, mcp.Path), ServerName: config.MCPServerName(controller.appName), Client: client, Token: token, Grants: grants, Tools: mcp.Tools(),
		IPsRestricted: controller.ipsRestricted, Unencrypted: controller.agents.Unencrypted(r),
	})

	if requests.IsHTMX(r) {
		controller.views.RenderFragment(w, r, "agents", "mcp-settings", view)

		return
	}

	controller.views.Render(w, r, http.StatusOK, "agents", view)
}

// NewAgentToken replaces the personal token and shows the new one.
func (controller *Controllers) NewAgentToken(w http.ResponseWriter, r *http.Request) {
	token, err := controller.agents.NewPersonalToken(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.renderAgents(w, r, "other", token)
}

// DisconnectAgent revokes a connected agent or the personal token.
func (controller *Controllers) DisconnectAgent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("grantID"), 10, 64)

	if err != nil || id <= 0 {
		http.NotFound(w, r)

		return
	}

	err = controller.agents.Disconnect(r.Context(), id)

	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		controller.serverError(w, r, err)
	default:
		redirectAfterPost(w, r, "/agents?client="+presenters.AgentClient(r.PostFormValue("client")))
	}
}
