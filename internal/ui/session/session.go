// Package session is the login of the web UI: sessions and their cookies,
// the limit on failed logins (per address and per username), changing your
// own password and the form tokens of pages. It is the UI's own part; the
// rules about users and their passwords are in package services.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/server/access"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

const (
	sessionCookie = "em_session"

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

// loginLimiter counts failed logins per key (a client address, or a
// username) in a fixed window.
type loginLimiter struct {
	mutex    sync.Mutex
	failures map[string]*loginWindowState
}

type loginWindowState struct {
	start time.Time
	count int
}

// maxLimiterKeys bounds the memory a flood of made-up usernames can take;
// past it new keys are not counted until windows end.
const maxLimiterKeys = 100_000

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string]*loginWindowState{}}
}

func (limiter *loginLimiter) blocked(key string, now time.Time) bool {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	state := limiter.failures[key]

	return state != nil && now.Sub(state.start) < LoginWindow && state.count >= MaxLoginFailures
}

// fail counts a failed login and reports whether it used up the last
// allowed attempt, so the key is blocked from now on.
func (limiter *loginLimiter) fail(key string, now time.Time) bool {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	// Drop finished windows so the map does not grow with every key ever seen.
	for other, state := range limiter.failures {
		if now.Sub(state.start) >= LoginWindow {
			delete(limiter.failures, other)
		}
	}

	state := limiter.failures[key]

	if state == nil {
		if len(limiter.failures) >= maxLimiterKeys {
			return false
		}

		state = &loginWindowState{start: now}
		limiter.failures[key] = state
	}

	state.count++

	return state.count == MaxLoginFailures
}

func (limiter *loginLimiter) reset(key string) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	delete(limiter.failures, key)
}

// usernameKey is the username as the limit counts it: in any case (usernames
// are unique in any case), capped so a long made-up one costs little memory.
func usernameKey(username string) string {
	key := strings.ToLower(strings.TrimSpace(username))

	if len(key) > config.MaxUsername {
		key = key[:config.MaxUsername]
	}

	return key
}

// IsHTTPS reports whether the browser talks to us over HTTPS.
func IsHTTPS(r *http.Request, trustedProxies []netip.Prefix) bool {
	return access.IsHTTPS(r, trustedProxies)
}

// Auth is the login of the panel's users: sessions and the limit on failed logins.
type Auth struct {
	logger         *slog.Logger
	store          *store.Store
	users          *services.Users // checks passwords
	trustedProxies []netip.Prefix
	sessionLife    time.Duration // SESSION_LIFETIME
	// Failed logins are limited per client address and per username, so
	// guessing one account's password from many addresses is limited too.
	byAddress  *loginLimiter
	byUsername *loginLimiter
	now        func() time.Time
	// opened: a superadmin exists, so the UI is open; it never stops
	// existing, so the database is asked only until then.
	opened atomic.Bool
}

func NewAuth(logger *slog.Logger, database *store.Store, settings *config.Config) *Auth {
	return &Auth{logger: logger, store: database, users: services.NewUsers(logger, database), trustedProxies: settings.TrustedProxies, sessionLife: settings.SessionLifetime, byAddress: newLoginLimiter(), byUsername: newLoginLimiter(), now: time.Now}
}

// Disabled: there is no superadmin yet (ADMIN_PASSWORD was never set), so
// nobody can log in.
func (auth *Auth) Disabled(ctx context.Context) bool {
	if auth.opened.Load() {
		return false
	}

	_, err := auth.store.Superadmin(ctx)
	auth.opened.Store(err == nil)

	return errors.Is(err, store.ErrNotFound)
}

// TooManyAttempts is what a blocked address is told, at login and when
// changing the password.
var TooManyAttempts = "Too many wrong passwords. Try again in " + config.HumanDuration(LoginWindow) + "."

// LoginResult is the outcome of a login attempt.
type LoginResult int

const (
	LoginOK               LoginResult = iota // a new session was started
	LoginWrongCredentials                    // the attempt was counted
	LoginBlocked                             // the address is blocked; the attempt was not checked
)

// Login checks the typed credentials of a login form and starts a new
// session when they match. Failed attempts are counted per client address
// and per username: after MaxLoginFailures within LoginWindow the address,
// or the username from any address, is blocked (unknown usernames too, so
// a block does not tell which exist). An error is a failure of the database.
func (auth *Auth) Login(w http.ResponseWriter, r *http.Request, username, password string) (LoginResult, error) {
	client := access.ClientIP(r, auth.trustedProxies)
	now := auth.now()

	if auth.blocked(client, username, now) {
		return LoginBlocked, nil
	}

	user, ok, err := auth.users.Authenticate(r.Context(), username, password)

	if err != nil {
		return LoginWrongCredentials, err
	}

	if !ok {
		auth.failed(client, username, now)

		return LoginWrongCredentials, nil
	}

	auth.byAddress.reset(client.String())
	auth.byUsername.reset(usernameKey(username))

	// A new session every login.
	if err := auth.startSession(r.Context(), w, r, user.ID, now); err != nil {
		return LoginWrongCredentials, err
	}

	return LoginOK, auth.store.RecordLogin(r.Context(), user.ID, now)
}

// blocked: the address or the username used up its attempts.
func (auth *Auth) blocked(client netip.Addr, username string, now time.Time) bool {
	return auth.byAddress.blocked(client.String(), now) || auth.byUsername.blocked(usernameKey(username), now)
}

// failed counts a wrong password from client for username. A single one is
// a typo, not an event: only a block (likely password guessing) is logged.
// The password typed is never logged, it may be one entered in the wrong field.
func (auth *Auth) failed(client netip.Addr, username string, now time.Time) {
	if auth.byAddress.fail(client.String(), now) {
		auth.logger.Warn("too many wrong passwords, address blocked", "client", client.String(), "attempts", MaxLoginFailures, "for", LoginWindow)
	}

	if auth.byUsername.fail(usernameKey(username), now) {
		auth.logger.Warn("too many wrong passwords, username blocked", "username", usernameKey(username), "client", client.String(), "attempts", MaxLoginFailures, "for", LoginWindow)
	}
}

// ChangePassword replaces the viewer's own password after checking the
// current one, which counts against the same limit as logins (a stolen
// session must not become a way to guess the password). The viewer's other
// sessions end; their agents stay connected, since it is the owner who
// changes it.
func (auth *Auth) ChangePassword(r *http.Request, viewer store.User, current, password, confirm string) (services.Problems, error) {
	client := access.ClientIP(r, auth.trustedProxies)
	now := auth.now()

	if auth.blocked(client, viewer.Username, now) {
		return services.Problems{"current_password": TooManyAttempts}, nil
	}

	problems, wrongCurrent, err := auth.users.ChangeOwnPassword(r.Context(), viewer, current, password, confirm, SessionHash(r))

	if wrongCurrent {
		auth.failed(client, viewer.Username, now)
	}

	return problems, err
}

// startSession creates a new session and sets its cookie. A session cookie
// the browser already had (possibly planted) is ended, never reused.
func (auth *Auth) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, now time.Time) error {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := auth.store.DeleteSession(ctx, hashToken(cookie.Value)); err != nil {
			auth.logger.Error("delete previous session", "err", err)
		}
	}

	token := newToken()
	expires := now.Add(auth.sessionLife)

	if err := auth.store.CreateSession(ctx, hashToken(token), userID, now, expires); err != nil {
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

// SessionUser returns the user of the request's session; false without a
// valid session.
func (auth *Auth) SessionUser(r *http.Request) (store.User, bool) {
	hash := SessionHash(r)

	if hash == "" {
		return store.User{}, false
	}

	user, err := auth.store.SessionUser(r.Context(), hash, auth.now())

	if err != nil && !errors.Is(err, store.ErrNotFound) {
		auth.logger.Error("check session", "err", err)
	}

	return user, err == nil
}

// SessionHash identifies the request's session as stored ("" without one).
func SessionHash(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)

	if err != nil || cookie.Value == "" {
		return ""
	}

	return hashToken(cookie.Value)
}

// FormToken binds an authorization form to this session and request.
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
