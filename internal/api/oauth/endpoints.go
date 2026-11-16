package oauth

import (
	"encoding/json"
	"net/http"
)

const maxBody = 16 << 10

// Requests per client address and minute to the open endpoints: agents
// register once and refresh about hourly, so these leave plenty of room.
const (
	registrationsPerMinute = 30
	tokenRequestsPerMinute = 120
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Register adds the open endpoints of the flow to router, each wrapped with
// protect (the IP allowlist). The consent page is the UI's.
//
// Agents look for the metadata in several places (RFC 8414 and 9728 put the
// path after /.well-known/, OpenID Connect before it), so every one of them
// answers, with or without the path of the MCP endpoint.
func (server *Server) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	// Registration and tokens are open to anyone the allowlist lets in, so
	// they are rate limited per client address.
	routes := map[string]http.Handler{
		"GET /.well-known/oauth-protected-resource":    http.HandlerFunc(server.protectedResourceMetadata),
		"GET /.well-known/oauth-protected-resource/":   http.HandlerFunc(server.protectedResourceMetadata),
		"GET /.well-known/oauth-authorization-server":  http.HandlerFunc(server.authorizationServerMetadata),
		"GET /.well-known/oauth-authorization-server/": http.HandlerFunc(server.authorizationServerMetadata),
		"GET /.well-known/openid-configuration":        http.HandlerFunc(server.authorizationServerMetadata),
		"GET /.well-known/openid-configuration/":       http.HandlerFunc(server.authorizationServerMetadata),
		"GET " + jwksPath:                              http.HandlerFunc(server.jwks),
		"POST " + RegisterPath:                         server.limit(registrationsPerMinute, server.register),
		"POST " + TokenPath:                            server.limit(tokenRequestsPerMinute, server.token),
	}

	for pattern, route := range routes {
		router.Handle(pattern, protect(server.guard(route)))
	}
}
