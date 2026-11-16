package ingest

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cors lets browser SDKs send events from allowed origins. Backend SDKs send
// no Origin header and are not affected.
type cors struct {
	anyOrigin bool
	origins   map[string]bool
}

func newCORS(allowedOrigins []string) cors {
	result := cors{origins: map[string]bool{}}

	for _, origin := range allowedOrigins {
		if origin == "*" {
			result.anyOrigin = true
		}

		result.origins[strings.ToLower(origin)] = true
	}

	return result
}

// apply sets the CORS response headers and reports whether the origin is allowed.
func (policy cors) apply(w http.ResponseWriter, r *http.Request) bool {
	header := w.Header()
	header.Add("Vary", "Origin")

	origin := r.Header.Get("Origin")

	if origin == "" {
		return true
	}

	if !policy.anyOrigin && !policy.origins[strings.ToLower(origin)] {
		return false
	}

	header.Set("Access-Control-Allow-Origin", origin)
	header.Set("Access-Control-Expose-Headers", "X-Sentry-Error, X-Sentry-Rate-Limits, Retry-After")

	if r.Method == http.MethodOptions {
		header.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding, X-Sentry-Auth, Authorization, Sentry-Trace, Baggage")
		header.Set("Access-Control-Max-Age", "3600")
	}

	return true
}

// rateLimiter allows up to limit envelopes per project per calendar minute.
// A fixed window is enough here: the goal is to stop a flood (e.g. a frontend
// error in a render loop on many clients), not to shape traffic precisely.
type rateLimiter struct {
	mutex   sync.Mutex
	limit   int
	windows map[int]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(limit int) *rateLimiter {
	return &rateLimiter{limit: limit, windows: map[int]*rateWindow{}}
}

// allow counts one envelope. When the limit is reached it returns false and
// the number of seconds until the next window.
func (limiter *rateLimiter) allow(projectID int, now time.Time) (retryAfter string, allowed bool) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	windowStart := now.Truncate(time.Minute)
	window := limiter.windows[projectID]

	if window == nil || !window.start.Equal(windowStart) {
		window = &rateWindow{start: windowStart}
		limiter.windows[projectID] = window
	}

	if window.count >= limiter.limit {
		seconds := int(windowStart.Add(time.Minute).Sub(now).Seconds()) + 1
		return strconv.Itoa(seconds), false
	}

	window.count++

	return "", true
}
