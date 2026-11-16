// Package oauth lets coding agents connect to the MCP server: an OAuth 2.1
// authorization server for public clients (authorization code with PKCE,
// rotating refresh tokens), and the personal token for agents that take a
// header instead.
//
// The flow, as Claude Code or Codex run it:
//  1. /mcp answers 401 with WWW-Authenticate pointing to the protected
//     resource metadata (RFC 9728), which names this server as the
//     authorization server; its metadata (RFC 8414) lists the endpoints.
//  2. The agent registers itself (RFC 7591, POST /oauth/register).
//  3. It opens /oauth/authorize in the browser; the admin logs in to the UI
//     and allows the agent on the consent page (package ui), which sends the
//     browser back to the agent with a code.
//  4. The agent trades the code for an access token (an hour) and a refresh
//     token (30 days, replaced at every use) at POST /oauth/token.
//
// Every connected agent is a grant, listed on the MCP page of the UI, where it
// can be disconnected. Only SHA-256 hashes of codes and tokens are stored, and
// nothing is kept in memory, so the UI and the MCP endpoint each use their own
// Server over the same database.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/access"
	"github.com/devforth/exception-monitoring/internal/store"
)

// Paths of the endpoints, below the server's address (PUBLIC_URL).
const (
	AuthorizePath = "/oauth/authorize" // the consent page, served by the UI
	TokenPath     = "/oauth/token"
	RegisterPath  = "/oauth/register"
	jwksPath      = "/oauth/jwks"
)

const (
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	codeTTL         = 5 * time.Minute
)

// Token prefixes tell the kinds apart in logs and secret scanners.
const (
	accessPrefix   = "emcp_at_"
	refreshPrefix  = "emcp_rt_"
	personalPrefix = "emcp_pt_"
)

type Server struct {
	Store *store.Store
	// Name (APP_NAME) is how agents show this server.
	Name string
	// PublicURL and TrustedProxies give the server's address, as for DSNs.
	PublicURL      string
	TrustedProxies []netip.Prefix
	// ResourcePath is the MCP endpoint the tokens are for.
	ResourcePath string
	Now          func() time.Time // nil means time.Now
}

func (server *Server) now() time.Time {
	if server.Now != nil {
		return server.Now()
	}

	return time.Now()
}

// Issuer identifies this authorization server: the server's address.
func (server *Server) Issuer(r *http.Request) string {
	return access.PublicURL(r, server.PublicURL, server.TrustedProxies).String()
}

// URL is the address of path on this server.
func (server *Server) URL(r *http.Request, path string) string {
	return access.PublicURL(r, server.PublicURL, server.TrustedProxies).JoinPath(path).String()
}

// ResourceURL is the MCP endpoint, as agents connect to it.
func (server *Server) ResourceURL(r *http.Request) string {
	return server.URL(r, server.ResourcePath)
}

// Error is an OAuth error response (RFC 6749 §5.2).
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (err *Error) Error() string { return err.Code + ": " + err.Description }

func oauthError(code, description string) *Error {
	return &Error{Code: code, Description: description}
}

// Codes and tokens are secretBytes random bytes, written in base64url.
const secretBytes = 32

var secretLength = base64.RawURLEncoding.EncodedLen(secretBytes)

// newSecret returns a random token with prefix.
func newSecret(prefix string) (string, error) {
	random := make([]byte, secretBytes)

	if _, err := rand.Read(random); err != nil {
		return "", err
	}

	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(sum[:])
}

func validSecret(secret, prefix string) bool {
	if !strings.HasPrefix(secret, prefix) || len(secret) != len(prefix)+secretLength {
		return false
	}

	random, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(secret, prefix))

	return err == nil && len(random) == secretBytes
}

// Authenticate returns the grant of a bearer token (an OAuth access token or
// the personal token) and records its use; store.ErrNotFound when it is
// unknown, expired or disconnected.
func (server *Server) Authenticate(ctx context.Context, token string) (store.Grant, error) {
	if !validSecret(token, accessPrefix) && !validSecret(token, personalPrefix) {
		return store.Grant{}, store.ErrNotFound
	}

	return server.Store.UseGrant(ctx, hash(token), server.now())
}

// Protect lets requests with a valid bearer token through. Others get 401
// with the address of the protected resource metadata, where agents find how
// to sign in.
func (server *Server) Protect(next http.Handler) http.Handler {
	next = withGrant(next)

	return server.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		scheme, token, found := strings.Cut(header, " ")
		challenge := `Bearer resource_metadata="` + server.URL(r, "/.well-known/oauth-protected-resource"+server.ResourcePath) + `"`

		if !found || !strings.EqualFold(scheme, "Bearer") || len(r.Header.Values("Authorization")) != 1 || strings.TrimSpace(token) == "" {
			unauthorized(w, challenge, "sign in to connect: see the MCP page of the panel")

			return
		}

		grant, err := server.Authenticate(r.Context(), strings.TrimSpace(token))

		switch {
		case errors.Is(err, store.ErrNotFound):
			unauthorized(w, challenge+`, error="invalid_token"`, "the token is invalid, expired or disconnected")
		case err != nil:
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, grant)))
		}
	}))
}

func unauthorized(w http.ResponseWriter, challenge, message string) {
	w.Header().Set("WWW-Authenticate", challenge)
	writeJSON(w, http.StatusUnauthorized, oauthError("invalid_token", message))
}

// NewPersonalToken replaces the user's personal token and returns the new
// one; it is shown once, only its hash is stored.
func (server *Server) NewPersonalToken(ctx context.Context, userID int64) (string, error) {
	token, err := newSecret(personalPrefix)

	if err != nil {
		return "", err
	}

	return token, server.Store.ReplacePersonalToken(ctx, userID, hash(token), server.now())
}

// Grants lists the connected agents and the personal token of a user,
// newest first.
func (server *Server) Grants(ctx context.Context, userID int64) ([]store.Grant, error) {
	return server.Store.ListGrants(ctx, userID, server.now())
}

// Grant reads a grant, to check whose it is.
func (server *Server) Grant(ctx context.Context, grantID int64) (store.Grant, error) {
	return server.Store.GetGrant(ctx, grantID)
}

// Disconnect revokes a grant: its tokens stop working at once.
func (server *Server) Disconnect(ctx context.Context, grantID int64) error {
	return server.Store.DeleteGrant(ctx, grantID)
}

// Unencrypted reports whether agents reach the server over plain HTTP on
// another machine: codes and tokens could then be read on the way.
func (server *Server) Unencrypted(r *http.Request) bool {
	address := access.PublicURL(r, server.PublicURL, server.TrustedProxies)

	return address.Scheme != "https" && !loopback(address)
}

// sameURL compares two URLs ignoring a trailing slash: agents differ in
// whether they add one to the resource.
func sameURL(first, second string) bool {
	return strings.TrimRight(first, "/") == strings.TrimRight(second, "/")
}

// checkResource accepts a missing resource parameter (RFC 8707) or this MCP
// endpoint.
func (server *Server) checkResource(r *http.Request, resource string) error {
	if resource != "" && !sameURL(resource, server.ResourceURL(r)) {
		return oauthError("invalid_target", "resource must be "+server.ResourceURL(r))
	}

	return nil
}

// loopback reports whether a redirect goes back to the agent on this
// machine (RFC 8252): http is fine there, and the port may change.
func loopback(redirect *url.URL) bool {
	host := redirect.Hostname()

	if host == "localhost" {
		return true
	}

	address, err := netip.ParseAddr(host)

	return err == nil && address.IsLoopback()
}
