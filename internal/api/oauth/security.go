package oauth

import (
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devforth/exception-monitoring/internal/server/access"
)

// guard protects every OAuth endpoint and MCP itself. Native agents omit
// Origin; browser requests must use this server's public origin.
func (server *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Add("Vary", "Origin")

		if !server.sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (server *Server) sameOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")

	if len(origins) == 0 {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}

	if len(origins) != 1 {
		return false
	}

	origin, err := url.Parse(origins[0])

	if err != nil || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || strings.Contains(origins[0], "#") {
		return false
	}

	public := access.PublicURL(r, server.PublicURL, server.TrustedProxies)

	return origin.Scheme == public.Scheme && strings.EqualFold(origin.Hostname(), public.Hostname()) && originPort(origin) == originPort(public)
}

func originPort(origin *url.URL) string {
	if port := origin.Port(); port != "" {
		return port
	}

	if origin.Scheme == "https" {
		return "443"
	}

	return "80"
}

// requestLimit bounds per-address and total traffic. Its address map is
// capped and reset each minute, so arbitrary addresses cannot grow it.
const (
	limitAddresses = 256 // addresses counted per minute
	limitTotal     = 10  // all addresses together: this many times perMinute
)

type requestLimit struct {
	mutex     sync.Mutex
	start     time.Time
	counts    map[netip.Addr]int
	total     int
	perMinute int
}

func (server *Server) limit(perMinute int, next http.HandlerFunc) http.Handler {
	limit := &requestLimit{perMinute: perMinute}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limit.allow(access.ClientIP(r, server.TrustedProxies), server.now()) {
			w.Header().Set("Retry-After", strconv.Itoa(int(time.Minute.Seconds()))) // the window ends within a minute
			writeJSON(w, http.StatusTooManyRequests, oauthError("temporarily_unavailable", "too many requests: try again later"))

			return
		}

		next(w, r)
	})
}

func (limit *requestLimit) allow(address netip.Addr, now time.Time) bool {
	limit.mutex.Lock()
	defer limit.mutex.Unlock()

	start := now.Truncate(time.Minute)

	if !limit.start.Equal(start) {
		limit.start, limit.total = start, 0
		limit.counts = make(map[netip.Addr]int)
	}

	count := limit.counts[address]

	if limit.total >= limitTotal*limit.perMinute || count >= limit.perMinute || (count == 0 && len(limit.counts) >= limitAddresses) {
		return false
	}

	limit.counts[address]++
	limit.total++

	return true
}

func uniqueParams(params url.Values) *Error {
	for name, values := range params {
		if len(values) != 1 {
			return oauthError("invalid_request", "duplicate parameter: "+name)
		}
	}

	return nil
}

// PKCE verifiers are 43 to 128 unreserved ASCII characters (RFC 7636).
const minVerifier, maxVerifier = 43, 128

func validVerifier(verifier string) bool {
	if len(verifier) < minVerifier || len(verifier) > maxVerifier {
		return false
	}

	for _, character := range verifier {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && !strings.ContainsRune("-._~", character) {
			return false
		}
	}

	return true
}

func validPort(port string) bool {
	if port == "" {
		return true
	}

	number, err := strconv.Atoi(port)

	return err == nil && number > 0 && number <= 65535
}
