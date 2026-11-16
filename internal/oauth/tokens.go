package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/devforth/exception-monitoring/internal/store"
)

// token trades a code or a refresh token for new tokens.
func (server *Server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_request", "the body must be a form"))

		return
	}

	if err := uniqueParams(r.PostForm); err != nil {
		writeJSON(w, http.StatusBadRequest, err)
		return
	}

	var (
		response map[string]any
		err      error
	)

	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		response, err = server.redeemCode(r)
	case "refresh_token":
		response, err = server.refresh(r)
	default:
		err = oauthError("unsupported_grant_type", "use authorization_code or refresh_token")
	}

	var oauthErr *Error

	switch {
	case errors.As(err, &oauthErr):
		writeJSON(w, http.StatusBadRequest, oauthErr)
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, response)
	}
}

func (server *Server) redeemCode(r *http.Request) (map[string]any, error) {
	form := r.PostForm

	if err := server.checkResource(r, form.Get("resource")); err != nil {
		return nil, err
	}

	code, err := server.Store.TakeOAuthCode(r.Context(), hash(form.Get("code")), server.now())

	if errors.Is(err, store.ErrNotFound) {
		return nil, oauthError("invalid_grant", "the code is invalid, used or expired: connect again")
	}

	if err != nil {
		return nil, err
	}

	if code.ClientID != form.Get("client_id") || code.RedirectURI != form.Get("redirect_uri") {
		return nil, oauthError("invalid_grant", "the code was issued for another client_id or redirect_uri")
	}

	if !validVerifier(form.Get("code_verifier")) || subtle.ConstantTimeCompare([]byte(pkceChallenge(form.Get("code_verifier"))), []byte(code.CodeChallenge)) != 1 {
		return nil, oauthError("invalid_grant", "code_verifier does not match the code challenge")
	}

	client, err := server.Store.GetOAuthClient(r.Context(), code.ClientID)

	if err != nil {
		return nil, err
	}

	access, refresh, tokens, err := server.newTokens()

	if err != nil {
		return nil, err
	}

	grant := store.Grant{UserID: code.UserID, Kind: store.GrantOAuth, ClientID: client.ID, Name: client.Name, CreatedAt: server.now()}

	if _, err := server.Store.CreateGrant(r.Context(), grant, tokens); err != nil {
		return nil, err
	}

	return tokenResponse(access, refresh), nil
}

// refresh replaces both tokens; the refresh token presented stops working,
// so a stolen one is useful once at most and the agent then fails to refresh.
func (server *Server) refresh(r *http.Request) (map[string]any, error) {
	form := r.PostForm

	if err := server.checkResource(r, form.Get("resource")); err != nil {
		return nil, err
	}

	presented := form.Get("refresh_token")

	if !validSecret(presented, refreshPrefix) {
		return nil, oauthError("invalid_grant", "the refresh token is invalid")
	}

	access, refresh, tokens, err := server.newTokens()

	if err != nil {
		return nil, err
	}

	err = server.Store.RotateGrant(r.Context(), form.Get("client_id"), hash(presented), tokens, server.now())

	if errors.Is(err, store.ErrNotFound) {
		return nil, oauthError("invalid_grant", "the refresh token is invalid, used or disconnected: connect again")
	}

	if err != nil {
		return nil, err
	}

	return tokenResponse(access, refresh), nil
}

func (server *Server) newTokens() (access, refresh string, tokens store.GrantTokens, err error) {
	if access, err = newSecret(accessPrefix); err != nil {
		return "", "", tokens, err
	}

	if refresh, err = newSecret(refreshPrefix); err != nil {
		return "", "", tokens, err
	}

	now := server.now()
	tokens = store.GrantTokens{
		AccessHash: hash(access), AccessExpiresAt: now.Add(accessTokenTTL),
		RefreshHash: hash(refresh), RefreshExpiresAt: now.Add(refreshTokenTTL),
	}

	return access, refresh, tokens, nil
}

func tokenResponse(access, refresh string) map[string]any {
	return map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL / time.Second),
		"refresh_token": refresh,
	}
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}
