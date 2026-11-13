package requests

import (
	"net/http"
	"strings"
)

// IsHTMX reports whether htmx sent the request (a swap, not a full page load).
func IsHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// SafeNext keeps a redirect target from the request (the post-login ?next=,
// the "return" of bulk actions) on this site: only a local path is accepted,
// so a crafted link cannot send the user elsewhere.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/issues"
	}

	return next
}
