package contracts

import "net/url"

// AuthorizeView names the agent and preserves its validated OAuth request.
type AuthorizeView struct {
	ClientName string
	// ClientSite: the host of the agent's metadata document; empty for a
	// registered agent.
	ClientSite  string
	RedirectURI string
	Params      url.Values
	CSRFToken   string
	Error       string
}
