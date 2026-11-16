package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// maxRedirectURIs and maxClientName bound what an open endpoint stores.
const (
	maxRedirectURIs = 10
	maxClientName   = 100
)

// register is dynamic client registration (RFC 7591) for public clients:
// agents on a developer's machine cannot keep a client secret.
func (server *Server) register(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))

	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_client_metadata", "the body must be JSON client metadata"))
		return
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_client_metadata", "the body must contain one JSON object"))
		return
	}

	if len(request.RedirectURIs) == 0 || len(request.RedirectURIs) > maxRedirectURIs {
		writeJSON(w, http.StatusBadRequest, oauthError("invalid_redirect_uri", "give 1 to 10 redirect_uris"))

		return
	}

	for _, uri := range request.RedirectURIs {
		if err := checkRedirectURI(uri); err != nil {
			writeJSON(w, http.StatusBadRequest, err)

			return
		}
	}

	name := clientName(request.ClientName)

	random := make([]byte, 16)

	if _, err := rand.Read(random); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	client := store.OAuthClient{ID: hex.EncodeToString(random), Name: name, RedirectURIs: request.RedirectURIs, CreatedAt: server.now()}

	if err := server.Store.RegisterOAuthClient(r.Context(), client); err != nil {
		if errors.Is(err, store.ErrOAuthCapacity) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, oauthError("temporarily_unavailable", "too many registered clients: try again later"))

			return
		}

		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// checkRedirectURI follows the same policy as AdminForth/Tracklify:
// HTTPS callbacks, or HTTP callbacks to a native client on loopback.
func checkRedirectURI(raw string) *Error {
	redirect, err := url.Parse(raw)

	if err != nil || redirect.Scheme == "" || redirect.Fragment != "" || strings.Contains(raw, "#") || redirect.User != nil || redirect.Opaque != "" ||
		strings.ContainsAny(raw, "\\\r\n\t") {
		return oauthError("invalid_redirect_uri", "redirect_uri must be an absolute URL without credentials or a fragment")
	}

	switch redirect.Scheme {
	case "https", "http":
		if redirect.Hostname() == "" || !validPort(redirect.Port()) || strings.ContainsAny(redirect.Host, " ;'\"<>*") {
			return oauthError("invalid_redirect_uri", "redirect_uri must have a valid host and port")
		}

		if redirect.Scheme == "http" && !loopback(redirect) {
			return oauthError("invalid_redirect_uri", "http redirect_uris must point to localhost")
		}

		return nil
	}

	return oauthError("invalid_redirect_uri", "use https or loopback http for the callback")
}

// redirectMatches compares a requested redirect_uri with a registered one:
// exactly, except the port of a loopback address (RFC 8252 §7.3).
func redirectMatches(requested, registered string) bool {
	if requested == registered {
		return true
	}

	first, firstErr := url.Parse(requested)
	second, secondErr := url.Parse(registered)

	if firstErr != nil || secondErr != nil || first.Scheme != "http" || second.Scheme != "http" || !loopback(first) || !loopback(second) {
		return false
	}

	first.Host = first.Hostname()
	second.Host = second.Hostname()

	return first.String() == second.String()
}
