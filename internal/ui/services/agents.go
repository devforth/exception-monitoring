package services

import (
	"context"
	"net/http"
	"net/url"

	"github.com/devforth/exception-monitoring/internal/oauth"
	"github.com/devforth/exception-monitoring/internal/store"
)

// Agents: the coding agents connected to the MCP server, the personal token
// and the consent page (package oauth does the work).
type Agents struct {
	authorization *oauth.Server
}

func NewAgents(authorization *oauth.Server) *Agents {
	return &Agents{authorization: authorization}
}

// CheckRequest validates an authorization request for the consent page.
func (agents *Agents) CheckRequest(ctx context.Context, r *http.Request, params url.Values) (oauth.Request, error) {
	return agents.authorization.CheckRequest(ctx, r, params)
}

// Decide checks the request again, as posted back by the consent form, and
// returns where to send the browser: with a code when allowed.
func (agents *Agents) Decide(ctx context.Context, r *http.Request, params url.Values, allow bool) (string, error) {
	request, err := agents.CheckRequest(ctx, r, params)

	if err != nil {
		return "", err
	}

	if allow {
		return agents.authorization.Approve(ctx, r, request)
	}

	return agents.authorization.Deny(r, request)
}

func (agents *Agents) Grants(ctx context.Context) ([]store.Grant, error) {
	return agents.authorization.Grants(ctx)
}

// NewPersonalToken replaces the personal token; the new one is shown once.
func (agents *Agents) NewPersonalToken(ctx context.Context) (string, error) {
	return agents.authorization.NewPersonalToken(ctx)
}

func (agents *Agents) Disconnect(ctx context.Context, grantID int64) error {
	return agents.authorization.Disconnect(ctx, grantID)
}

// Unencrypted: agents reach the server over plain HTTP on another machine.
func (agents *Agents) Unencrypted(r *http.Request) bool {
	return agents.authorization.Unencrypted(r)
}
