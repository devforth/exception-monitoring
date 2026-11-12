package ui

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/devforth/exception-monitoring/internal/access"
)

const (
	sessionCookie   = "em_session"
	sessionLifetime = 14 * 24 * time.Hour

	// Failed logins allowed per client address within loginWindow.
	maxLoginFailures = 10
	loginWindow      = 15 * time.Minute
)

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

func newToken() string {
	var token [32]byte

	_, _ = rand.Read(token[:])

	return hex.EncodeToString(token[:])
}

// credentialsIterations follows the OWASP recommendation for PBKDF2-SHA256.
// A variable only so tests can lower it (export_test.go): the race detector
// makes 600k iterations take seconds per test server.
var credentialsIterations = 600_000

// credentialsFingerprint identifies the current admin credentials. It is stored
// with each session, so changing the username or password invalidates all
// sessions. A slow key derivation keeps the password out of reach if the
// database file leaks. Computed once at startup.
func credentialsFingerprint(username, password string) (string, error) {
	if password == "" {
		return "", nil // UI disabled: nobody can log in, there is nothing to tie sessions to
	}

	key, err := pbkdf2.Key(sha256.New, password, []byte("exception-monitoring-session:"+username), credentialsIterations, 32)

	if err != nil {
		return "", fmt.Errorf("derive credentials fingerprint: %w", err)
	}

	return hex.EncodeToString(key), nil
}

// credentialsMatch compares both values in constant time and always checks
// both, so the response time does not tell which one was wrong.
func credentialsMatch(username, password, wantUsername, wantPassword string) bool {
	usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(wantUsername))
	passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(wantPassword))

	return usernameOK&passwordOK == 1
}

// loginLimiter counts failed logins per client address in a fixed window.
type loginLimiter struct {
	mutex    sync.Mutex
	failures map[netip.Addr]*loginWindowState
}

type loginWindowState struct {
	start time.Time
	count int
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[netip.Addr]*loginWindowState{}}
}

func (limiter *loginLimiter) blocked(address netip.Addr, now time.Time) bool {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	state := limiter.failures[address]

	return state != nil && now.Sub(state.start) < loginWindow && state.count >= maxLoginFailures
}

// fail counts a failed login and reports whether it used up the last
// allowed attempt, so the address is blocked from now on.
func (limiter *loginLimiter) fail(address netip.Addr, now time.Time) bool {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	// Drop finished windows so the map does not grow with every address ever seen.
	for key, state := range limiter.failures {
		if now.Sub(state.start) >= loginWindow {
			delete(limiter.failures, key)
		}
	}

	state := limiter.failures[address]

	if state == nil {
		state = &loginWindowState{start: now}
		limiter.failures[address] = state
	}

	state.count++

	return state.count == maxLoginFailures
}

func (limiter *loginLimiter) reset(address netip.Addr) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	delete(limiter.failures, address)
}

// isHTTPS reports whether the browser talks to us over HTTPS, directly or
// through a trusted reverse proxy that terminates TLS.
func (handler *Handler) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	return access.FromTrustedProxy(r, handler.trustedProxies) &&
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (handler *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   handler.isHTTPS(r),
		// Lax: the cookie is not sent with cross-site POSTs, which blocks CSRF
		// together with the Origin check in sameOrigin.
		SameSite: http.SameSiteLaxMode,
	})
}

func (handler *Handler) loggedIn(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)

	if err != nil || cookie.Value == "" {
		return false
	}

	valid, err := handler.store.SessionValid(r.Context(), hashToken(cookie.Value), handler.credentials, handler.now())

	if err != nil {
		handler.logger.Error("check session", "err", err)

		return false
	}

	return valid
}

// requireLogin sends anonymous visitors to the login page. For htmx requests
// a redirect would be swapped into the page, so HX-Redirect asks htmx to do a
// full navigation instead.
func (handler *Handler) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if handler.loggedIn(r) {
			next(w, r)

			return
		}

		target := "/login?next=" + url.QueryEscape(r.URL.RequestURI())

		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", target)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

// sameOrigin rejects state-changing requests sent from other sites (CSRF).
// Browsers always send Origin with POST; a request without it is not from a
// browser form on another site, so it is allowed.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
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

// safeNext keeps the post-login redirect on this site: only a local path is
// accepted, so a crafted ?next= link cannot send the user elsewhere.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/issues"
	}

	return next
}

func (handler *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if handler.loggedIn(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)

		return
	}

	handler.render(w, r, http.StatusOK, "login", loginData{Next: safeNext(r.URL.Query().Get("next")), Disabled: handler.adminPassword == ""})
}

type loginData struct {
	Next     string
	Error    string
	Username string
	Disabled bool
}

func (handler *Handler) login(w http.ResponseWriter, r *http.Request) {
	data := loginData{Next: safeNext(r.FormValue("next")), Username: r.FormValue("username"), Disabled: handler.adminPassword == ""}

	if data.Disabled {
		handler.render(w, r, http.StatusForbidden, "login", data)

		return
	}

	client := access.ClientIP(r, handler.trustedProxies)
	now := handler.now()

	if handler.limiter.blocked(client, now) {
		data.Error = "Too many failed attempts. Try again in 15 minutes."
		handler.render(w, r, http.StatusTooManyRequests, "login", data)

		return
	}

	if !credentialsMatch(r.FormValue("username"), r.FormValue("password"), handler.adminUsername, handler.adminPassword) {
		// A single wrong password is a typo, not an event: only a blocked
		// address (likely password guessing) is logged. What was typed is
		// never logged, it may be a password entered in the wrong field.
		if handler.limiter.fail(client, now) {
			handler.logger.Warn("too many failed UI logins, address blocked", "client", client.String(), "attempts", maxLoginFailures, "for", loginWindow)
		}

		data.Error = "Wrong username or password."
		handler.render(w, r, http.StatusUnauthorized, "login", data)

		return
	}

	handler.limiter.reset(client)

	// A new session every login; a session cookie the browser already had
	// (possibly planted) is ended, never reused.
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := handler.store.DeleteSession(r.Context(), hashToken(cookie.Value)); err != nil {
			handler.logger.Error("delete previous session", "err", err)
		}
	}

	token := newToken()
	expires := now.Add(sessionLifetime)

	if err := handler.store.CreateSession(r.Context(), hashToken(token), handler.credentials, now, expires); err != nil {
		handler.serverError(w, r, err)

		return
	}

	handler.setSessionCookie(w, r, token, expires)
	http.Redirect(w, r, data.Next, http.StatusSeeOther)
}

func (handler *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := handler.store.DeleteSession(r.Context(), hashToken(cookie.Value)); err != nil {
			handler.logger.Error("delete session", "err", err)
		}
	}

	handler.setSessionCookie(w, r, "", time.Unix(0, 0))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
