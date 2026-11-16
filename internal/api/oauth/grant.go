package oauth

import (
	"context"
	"net/http"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// The grant of a request reaches the MCP tools as the SDK's TokenInfo (the
// SDK copies it from the request into every tool call), so a tool knows for
// which user it acts. Protect checks the token and puts the grant into the
// request; the SDK's middleware, built once, only hands it on.

type grantKey struct{}

const tokenInfoGrant = "grant"

func withGrant(next http.Handler) http.Handler {
	return auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		grant, ok := ctx.Value(grantKey{}).(store.Grant)

		if !ok {
			return nil, auth.ErrInvalidToken
		}

		return &auth.TokenInfo{UserID: strconv.FormatInt(grant.UserID, 10), Extra: map[string]any{tokenInfoGrant: grant}}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)
}

// GrantOf returns the grant a tool call was authenticated with.
func GrantOf(info *auth.TokenInfo) (store.Grant, bool) {
	if info == nil {
		return store.Grant{}, false
	}

	grant, ok := info.Extra[tokenInfoGrant].(store.Grant)

	return grant, ok
}
