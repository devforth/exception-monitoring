package contracts

import "net/url"

// AuthorizeView names the agent and preserves its validated OAuth request.
type AuthorizeView struct {
	ClientName  string
	RedirectURI string
	Params      url.Values
	CSRFToken   string
	Error       string
}
