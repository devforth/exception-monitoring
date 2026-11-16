package oauth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"

	"github.com/devforth/exception-monitoring/internal/store"
)

// Request is an authorization request the consent page shows: which agent
// asks, and where the browser goes back to.
type Request struct {
	ClientID      string
	ClientName    string
	RedirectURI   string
	CodeChallenge string
	State         string
	// Params are the request's query parameters: the consent form posts
	// them back, and they are checked again then.
	Params url.Values
}

// ClientSite is the host that published the agent's metadata document,
// empty for a registered agent: unlike the name, which any agent can choose,
// it cannot be forged, so the consent page shows it.
func (request Request) ClientSite() string {
	if !isClientIDURL(request.ClientID) {
		return ""
	}

	parsed, _ := url.Parse(request.ClientID)

	return parsed.Hostname()
}

// RedirectOrigin is where the browser is sent back to, for the page's CSP.
func (request Request) RedirectOrigin() string {
	redirect, err := url.Parse(request.RedirectURI)

	if err != nil {
		return ""
	}

	if redirect.Host == "" {
		return redirect.Scheme + ":"
	}

	return redirect.Scheme + "://" + redirect.Host
}

// CheckRequest validates an authorization request. Its errors are shown on
// the page, not sent to the redirect_uri: until the client and its
// redirect_uri are known to match, the browser must not be sent there.
func (server *Server) CheckRequest(ctx context.Context, r *http.Request, params url.Values) (Request, error) {
	request := Request{
		ClientID: params.Get("client_id"), RedirectURI: params.Get("redirect_uri"),
		CodeChallenge: params.Get("code_challenge"), State: params.Get("state"), Params: params,
	}

	if err := uniqueParams(params); err != nil {
		return request, err
	}

	if err := checkRedirectURI(request.RedirectURI); err != nil {
		return request, err
	}

	client, err := server.client(ctx, request.ClientID)

	if err != nil {
		return request, err
	}

	request.ClientName = client.Name
	challenge, challengeErr := base64.RawURLEncoding.Strict().DecodeString(request.CodeChallenge)

	if !slices.ContainsFunc(client.RedirectURIs, func(registered string) bool { return redirectMatches(request.RedirectURI, registered) }) {
		return request, oauthError("invalid_request", "redirect_uri is not one the agent registered")
	}

	switch {
	case params.Get("response_type") != "code":
		return request, oauthError("unsupported_response_type", "only response_type=code is supported")
	case params.Get("code_challenge_method") != "S256" || challengeErr != nil || len(challenge) != 32:
		return request, oauthError("invalid_request", "PKCE with code_challenge_method=S256 is required")
	}

	return request, server.checkResource(r, params.Get("resource"))
}

// Approve issues a code for the agent to act as userID (the user who
// allowed it) and returns where to send the browser.
func (server *Server) Approve(ctx context.Context, r *http.Request, request Request, userID int64) (string, error) {
	code, err := newSecret("")

	if err != nil {
		return "", err
	}

	err = server.Store.CreateOAuthCode(ctx, hash(code), store.OAuthCode{
		UserID: userID, ClientID: request.ClientID, ClientName: request.ClientName, RedirectURI: request.RedirectURI, CodeChallenge: request.CodeChallenge,
		ExpiresAt: server.now().Add(codeTTL),
	}, server.now())

	if err != nil {
		return "", err
	}

	return server.redirect(r, request, url.Values{"code": {code}})
}

// Deny returns where to send the browser when the admin refuses.
func (server *Server) Deny(r *http.Request, request Request) (string, error) {
	return server.redirect(r, request, url.Values{"error": {"access_denied"}})
}

func (server *Server) redirect(r *http.Request, request Request, values url.Values) (string, error) {
	redirect, err := url.Parse(request.RedirectURI)

	if err != nil {
		return "", err
	}

	query := redirect.Query()

	for name, value := range values {
		query[name] = value
	}

	if request.State != "" {
		query.Set("state", request.State)
	}

	// RFC 9207: tells the agent which server answered.
	query.Set("iss", server.Issuer(r))
	redirect.RawQuery = query.Encode()

	return redirect.String(), nil
}
