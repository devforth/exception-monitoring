package requests

import (
	"net/http"
	"net/url"
	"strings"
)

// IsHTMX reports whether htmx sent the request (a swap, not a full page load).
func IsHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// SafeNext keeps a redirect target from the request (the post-login ?next=,
// the "return" of bulk actions) on this site: only a local path is accepted,
// so a crafted link cannot send the user elsewhere. Browsers drop tabs and
// newlines from a URL and read a backslash as "/" ("/<tab>/evil.com" opens
// evil.com), so control characters, spaces and backslashes are refused, and
// the rest must parse as a path without a scheme or host.
func SafeNext(next string) string {
	const fallback = "/issues"

	if strings.ContainsFunc(next, func(char rune) bool { return char <= ' ' || char == 0x7f || char == '\\' }) {
		return fallback
	}

	target, err := url.Parse(next)

	if err != nil || target.Scheme != "" || target.Host != "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return fallback
	}

	return next
}
