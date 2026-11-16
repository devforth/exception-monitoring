package oauth

import (
	"net/http"

	"github.com/devforth/exception-monitoring/internal/config"
)

func (server *Server) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	name := server.Name

	if name == "" {
		name = config.DefaultAppName
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 server.ResourceURL(r),
		"resource_name":            name,
		"authorization_servers":    []string{server.Issuer(r)},
		"bearer_methods_supported": []string{"header"},
	})
}

func (server *Server) authorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         server.Issuer(r),
		"authorization_endpoint":                         server.URL(r, AuthorizePath),
		"token_endpoint":                                 server.URL(r, TokenPath),
		"registration_endpoint":                          server.URL(r, RegisterPath),
		"client_id_metadata_document_supported":          true,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
		// OpenID Connect discovery requires these, and some agents validate
		// the document against it. No ID tokens are issued: the key set is empty.
		"jwks_uri":                              server.URL(r, jwksPath),
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (server *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}})
}
