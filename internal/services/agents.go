package services

import (
	"context"
	"net/http"
	"net/url"
	"slices"

	"github.com/devforth/exception-monitoring/internal/api/oauth"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Agents: the coding agents connected to the MCP server, the personal tokens
// and the consent page (package oauth does the work). Whose agents a viewer
// may see or disconnect is store.User.CanAccessAgentsOf.
type Agents struct {
	authorization *oauth.Server
	store         store.Store
}

func NewAgents(authorization *oauth.Server, database store.Store) *Agents {
	return &Agents{authorization: authorization, store: database}
}

// CheckRequest validates an authorization request for the consent page.
func (agents *Agents) CheckRequest(ctx context.Context, r *http.Request, params url.Values) (oauth.Request, error) {
	return agents.authorization.CheckRequest(ctx, r, params)
}

// Decide checks the request again, as posted back by the consent form, and
// returns where to send the browser: with a code when allowed, for the agent
// to act as the viewer.
func (agents *Agents) Decide(ctx context.Context, r *http.Request, viewer store.User, params url.Values, allow bool) (string, error) {
	request, err := agents.CheckRequest(ctx, r, params)

	if err != nil {
		return "", err
	}

	if allow {
		return agents.authorization.Approve(ctx, r, request, viewer.ID)
	}

	return agents.authorization.Deny(r, request)
}

// AgentsPage is what the MCP page lists.
type AgentsPage struct {
	Owner  store.User   // whose agents are listed
	Owners []store.User // whom the viewer may choose (only themselves for most)
	Grants []store.Grant
	// TokenSet: the viewer (whoever Owner is) has a personal token; the
	// token form is always their own.
	TokenSet bool
}

// Page lists the agents of ownerID when the viewer may see them, otherwise
// the viewer's own.
func (agents *Agents) Page(ctx context.Context, viewer store.User, ownerID string) (AgentsPage, error) {
	page := AgentsPage{Owner: viewer, Owners: []store.User{viewer}}

	if viewer.CanAdminister() {
		users, err := agents.store.ListUsers(ctx)

		if err != nil {
			return page, err
		}

		page.Owners = slices.DeleteFunc(users, func(user store.User) bool { return !viewer.CanAccessAgentsOf(user) })

		if index := slices.IndexFunc(page.Owners, func(user store.User) bool { return user.ID == ownerID }); index >= 0 {
			page.Owner = page.Owners[index]
		}
	}

	var err error

	if page.Grants, err = agents.authorization.Grants(ctx, page.Owner.ID); err != nil {
		return page, err
	}

	page.TokenSet, err = agents.store.HasPersonalToken(ctx, viewer.ID)

	return page, err
}

// NewPersonalToken replaces the viewer's personal token; the new one is
// shown once.
func (agents *Agents) NewPersonalToken(ctx context.Context, viewer store.User) (string, error) {
	return agents.authorization.NewPersonalToken(ctx, viewer.ID)
}

// Disconnect revokes an agent the viewer may access.
func (agents *Agents) Disconnect(ctx context.Context, viewer store.User, grantID string) error {
	grant, err := agents.authorization.Grant(ctx, grantID)

	if err != nil {
		return err
	}

	owner, err := agents.store.GetUser(ctx, grant.UserID)

	if err != nil {
		return err
	}

	if !viewer.CanAccessAgentsOf(owner) {
		return ErrForbidden
	}

	return agents.authorization.Disconnect(ctx, grantID)
}

// Unencrypted: agents reach the server over plain HTTP on another machine.
func (agents *Agents) Unencrypted(r *http.Request) bool {
	return agents.authorization.Unencrypted(r)
}
