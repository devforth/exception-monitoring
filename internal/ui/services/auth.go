// Package services is the logic behind the UI pages that is not HTTP, and
// the only way the pages reach the database: admin sessions and the login
// limit, issues, their events and actions, logs, project DSNs, colors and
// storage, notification connections (duplicates, default names, saving),
// the delivery log and email verification codes. Controllers call it; it
// returns data or the view types of contracts and never writes a response
// body (only the session cookie). A change of several steps is one method,
// so a controller cannot do half of it.
package services

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/devforth/exception-monitoring/internal/access"
	"github.com/devforth/exception-monitoring/internal/store"
)

const (
	sessionCookie   = "em_session"
	sessionLifetime = 14 * 24 * time.Hour

	// MaxLoginFailures failed logins are allowed per client address within LoginWindow.
	MaxLoginFailures = 10
	LoginWindow      = 15 * time.Minute
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

// CredentialsIterations follows the OWASP recommendation for PBKDF2-SHA256.
// A variable only so tests can lower it (internal/ui/export_test.go): the race detector
// makes 600k iterations take seconds per test server.
var CredentialsIterations = 600_000

// CredentialsFingerprint identifies the current admin credentials. It is stored
// with each session, so changing the username or password invalidates all
// sessions. A slow key derivation keeps the password out of reach if the
// database file leaks. Computed once at startup.
func CredentialsFingerprint(username, password string) (string, error) {
	if password == "" {
		return "", nil // UI disabled: nobody can log in, there is nothing to tie sessions to
	}

	key, err := pbkdf2.Key(sha256.New, password, []byte("exception-monitoring-session:"+username), CredentialsIterations, 32)

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

	return state != nil && now.Sub(state.start) < LoginWindow && state.count >= MaxLoginFailures
}

// fail counts a failed login and reports whether it used up the last
// allowed attempt, so the address is blocked from now on.
func (limiter *loginLimiter) fail(address netip.Addr, now time.Time) bool {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	// Drop finished windows so the map does not grow with every address ever seen.
	for key, state := range limiter.failures {
		if now.Sub(state.start) >= LoginWindow {
			delete(limiter.failures, key)
		}
	}

	state := limiter.failures[address]

	if state == nil {
		state = &loginWindowState{start: now}
		limiter.failures[address] = state
	}

	state.count++

	return state.count == MaxLoginFailures
}

func (limiter *loginLimiter) reset(address netip.Addr) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	delete(limiter.failures, address)
}

// IsHTTPS reports whether the browser talks to us over HTTPS.
func IsHTTPS(r *http.Request, trustedProxies []netip.Prefix) bool {
	return access.IsHTTPS(r, trustedProxies)
}

// Auth is the admin login: credentials, sessions and the limit on failed logins.
type Auth struct {
	logger        *slog.Logger
	store         *store.Store
	adminUsername string
	adminPassword string
	// credentials fingerprints the admin username and password; sessions are
	// valid only with the fingerprint they were created with.
	credentials    string
	trustedProxies []netip.Prefix
	limiter        *loginLimiter
	now            func() time.Time
}

func NewAuth(logger *slog.Logger, database *store.Store, adminUsername, adminPassword, credentials string, trustedProxies []netip.Prefix) *Auth {
	return &Auth{
		logger: logger, store: database, adminUsername: adminUsername, adminPassword: adminPassword,
		credentials: credentials, trustedProxies: trustedProxies, limiter: newLoginLimiter(), now: time.Now,
	}
}

// Disabled: no admin password is set, so nobody can log in.
func (auth *Auth) Disabled() bool {
	return auth.adminPassword == ""
}

// LoginResult is the outcome of a login attempt.
type LoginResult int

const (
	LoginOK               LoginResult = iota // a new session was started
	LoginWrongCredentials                    // the attempt was counted
	LoginBlocked                             // the address is blocked; the attempt was not checked
)

// Login checks the typed credentials of a login form and starts a new
// session when they match. Failed attempts are counted per client address:
// after MaxLoginFailures within LoginWindow the address is blocked. An error
// is a failure of the database.
func (auth *Auth) Login(w http.ResponseWriter, r *http.Request, username, password string) (LoginResult, error) {
	client := access.ClientIP(r, auth.trustedProxies)
	now := auth.now()

	if auth.limiter.blocked(client, now) {
		return LoginBlocked, nil
	}

	if !credentialsMatch(username, password, auth.adminUsername, auth.adminPassword) {
		// A single wrong password is a typo, not an event: only a blocked
		// address (likely password guessing) is logged. What was typed is
		// never logged, it may be a password entered in the wrong field.
		if auth.limiter.fail(client, now) {
			auth.logger.Warn("too many failed UI logins, address blocked", "client", client.String(), "attempts", MaxLoginFailures, "for", LoginWindow)
		}

		return LoginWrongCredentials, nil
	}

	auth.limiter.reset(client)

	// A new session every login.
	if err := auth.startSession(r.Context(), w, r, now); err != nil {
		return LoginWrongCredentials, err
	}

	return LoginOK, nil
}

// startSession creates a new session and sets its cookie. A session cookie
// the browser already had (possibly planted) is ended, never reused.
func (auth *Auth) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, now time.Time) error {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := auth.store.DeleteSession(ctx, hashToken(cookie.Value)); err != nil {
			auth.logger.Error("delete previous session", "err", err)
		}
	}

	token := newToken()
	expires := now.Add(sessionLifetime)

	if err := auth.store.CreateSession(ctx, hashToken(token), auth.credentials, now, expires); err != nil {
		return err
	}

	auth.setSessionCookie(w, r, token, expires)

	return nil
}

// EndSession deletes the session of the request and clears its cookie.
func (auth *Auth) EndSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := auth.store.DeleteSession(ctx, hashToken(cookie.Value)); err != nil {
			auth.logger.Error("delete session", "err", err)
		}
	}

	auth.setSessionCookie(w, r, "", time.Unix(0, 0))
}

func (auth *Auth) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   IsHTTPS(r, auth.trustedProxies),
		// Lax: the cookie is not sent with cross-site POSTs, which blocks CSRF
		// together with the Origin check in middleware.SameOrigin.
		SameSite: http.SameSiteLaxMode,
	})
}

// LoggedIn reports whether the request carries a valid session.
func (auth *Auth) LoggedIn(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)

	if err != nil || cookie.Value == "" {
		return false
	}

	valid, err := auth.store.SessionValid(r.Context(), hashToken(cookie.Value), auth.credentials, auth.now())

	if err != nil {
		auth.logger.Error("check session", "err", err)

		return false
	}

	return valid
}

// FormToken binds an authorization form to this admin session and request.
// The session cookie is random and HttpOnly, so another site cannot forge it.
func (auth *Auth) FormToken(r *http.Request, value string) string {
	cookie, err := r.Cookie(sessionCookie)

	if err != nil || cookie.Value == "" {
		return ""
	}

	mac := hmac.New(sha256.New, []byte(cookie.Value))
	_, _ = mac.Write([]byte(value))

	return hex.EncodeToString(mac.Sum(nil))
}

// ValidFormToken checks a session-bound token without exposing its value.
func (auth *Auth) ValidFormToken(r *http.Request, action, token string) bool {
	expected := auth.FormToken(r, action)

	return token != "" && expected != "" && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}
