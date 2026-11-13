// Package middleware wraps UI handlers: login required, same-origin POSTs
// (CSRF), security headers and caching of static files. The session and
// login logic they rely on lives in services.Auth.
package middleware

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/devforth/exception-monitoring/internal/ui/requests"
	"github.com/devforth/exception-monitoring/internal/ui/services"
)

// RequireLogin sends anonymous visitors to the login page. For htmx requests
// a redirect would be swapped into the page, so HX-Redirect asks htmx to do a
// full navigation instead.
func RequireLogin(auth *services.Auth) func(next http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if auth.LoggedIn(r) {
				next(w, r)

				return
			}

			target := "/login?next=" + url.QueryEscape(r.URL.RequestURI())

			if requests.IsHTMX(r) {
				w.Header().Set("HX-Redirect", target)
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			http.Redirect(w, r, target, http.StatusSeeOther)
		}
	}
}

// SameOrigin rejects state-changing requests sent from other sites (CSRF).
// Browsers always send Origin with POST; a request without it is not from a
// browser form on another site, so it is allowed.
func SameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)

			if err != nil || !strings.EqualFold(parsed.Host, r.Host) {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)

				return
			}
		}

		next(w, r)
	}
}

// StaticCache: pages link assets with ?v=<content hash> (see views.assetURL), so a
// versioned URL never changes content and can be cached for good. Without the
// version, the browser must revalidate, or it keeps an old app.css after an update.
func StaticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("v") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders: scripts and styles only from our own /static/ (no inline
// code), no framing by other sites (clickjacking), no MIME sniffing. Pages
// hold error data and must not stay in any cache (StaticCache overrides this
// for static files); the same URL serves a full page or an htmx fragment, so
// caches must tell them apart (Vary).
// ContentSecurityPolicy is the policy of every page. formTargets are other
// origins a form of the page may send the browser to (the OAuth consent
// page returns to the agent): browsers apply form-action to redirects too.
func ContentSecurityPolicy(formTargets ...string) string {
	formAction := strings.TrimSpace("'self' " + strings.Join(formTargets, " "))

	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action " +
		formAction + "; base-uri 'none'"
}

func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", ContentSecurityPolicy())
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Cache-Control", "no-store")
		header.Add("Vary", "HX-Request")
		next.ServeHTTP(w, r)
	})
}
