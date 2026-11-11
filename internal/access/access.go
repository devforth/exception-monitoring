// Package access protects the admin parts of the server (REST API, later the
// UI) with an IP allowlist and credentials. Ingest is not protected here: SDKs
// in browsers send from anywhere, and they authenticate with DSN keys.
package access

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address of the client that made the request.
//
// Behind a reverse proxy, RemoteAddr is the proxy. Only then the client comes
// from X-Forwarded-For: each proxy appends the address it received the request
// from, so the list is read from the right, skipping trusted proxies. The first
// untrusted address is the client. Entries to the left of it could have been
// written by the client itself, so they are never used.
func ClientIP(r *http.Request, trustedProxies []netip.Prefix) netip.Addr {
	remote := remoteAddr(r)
	if !contains(trustedProxies, remote) {
		return remote
	}
	forwarded := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		address, err := netip.ParseAddr(strings.TrimSpace(forwarded[index]))
		if err != nil {
			// A malformed entry: stop rather than trust anything to the left of it.
			return remote
		}
		address = address.Unmap()
		if !contains(trustedProxies, address) {
			return address
		}
	}
	return remote
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return address.Unmap()
}

func contains(networks []netip.Prefix, address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	for _, network := range networks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

// RequireAllowedIP rejects clients outside allowedIPs with 403. An empty list allows everyone.
func RequireAllowedIP(allowedIPs, trustedProxies []netip.Prefix, next http.Handler) http.Handler {
	if len(allowedIPs) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !contains(allowedIPs, ClientIP(r, trustedProxies)) {
			writeError(w, http.StatusForbidden, "access from this address is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireToken accepts requests with "Authorization: Bearer <token>".
// With an empty token every request is rejected: the API stays closed until
// API_TOKEN is configured.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeError(w, http.StatusUnauthorized, "API is disabled: set API_TOKEN")
			return
		}
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(header[len(prefix):])), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="exception-monitoring"`)
			writeError(w, http.StatusUnauthorized, "invalid or missing API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
