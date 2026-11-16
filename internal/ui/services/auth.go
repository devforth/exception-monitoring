// Package services is the logic behind the UI pages that is not HTTP, and
// the only way the pages reach the database: users, their sessions and the
// login limit, issues, their events and actions, logs, project DSNs, colors and
// storage, notification connections (duplicates, default names, saving),
// the delivery log and email verification codes. Controllers call it; it
// returns data or the view types of contracts and never writes a response
// body (only the session cookie). A change of several steps is one method,
// so a controller cannot do half of it.
package services

import (
	"cmp"
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

	"github.com/devforth/exception-monitoring/internal/access"
	"github.com/devforth/exception-monitoring/internal/config"
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

// Auth is the login of the panel's users: sessions and the limit on failed logins.
type Auth struct {
	logger         *slog.Logger
	store          *store.Store
	trustedProxies []netip.Prefix
	limiter        *loginLimiter
	now            func() time.Time
	// opened: a superadmin exists, so the UI is open; it never stops
	// existing, so the database is asked only until then.
	opened atomic.Bool
}

func NewAuth(logger *slog.Logger, database *store.Store, trustedProxies []netip.Prefix) *Auth {
	return &Auth{logger: logger, store: database, trustedProxies: trustedProxies, limiter: newLoginLimiter(), now: time.Now}
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
// session when they match. Failed attempts are counted per client address:
// after MaxLoginFailures within LoginWindow the address is blocked. An error
// is a failure of the database.
func (auth *Auth) Login(w http.ResponseWriter, r *http.Request, username, password string) (LoginResult, error) {
	client := access.ClientIP(r, auth.trustedProxies)
	now := auth.now()

	if auth.limiter.blocked(client, now) {
		return LoginBlocked, nil
	}

	user, hash, err := auth.store.FindLogin(r.Context(), strings.TrimSpace(username))

	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return LoginWrongCredentials, err
	}

	// An unknown username is checked against a dummy hash, so the response
	// time does not tell which usernames exist.
	if !checkPassword(cmp.Or(hash, dummyPasswordHash()), password) || user.ID == 0 {
		auth.failed(client, now)

		return LoginWrongCredentials, nil
	}

	auth.limiter.reset(client)

	// A new session every login.
	if err := auth.startSession(r.Context(), w, r, user.ID, now); err != nil {
		return LoginWrongCredentials, err
	}

	return LoginOK, auth.store.RecordLogin(r.Context(), user.ID, now)
}

// failed counts a wrong password from client. A single one is a typo, not an
// event: only a blocked address (likely password guessing) is logged. What
// was typed is never logged, it may be a password entered in the wrong field.
func (auth *Auth) failed(client netip.Addr, now time.Time) {
	if auth.limiter.fail(client, now) {
		auth.logger.Warn("too many wrong passwords, address blocked", "client", client.String(), "attempts", MaxLoginFailures, "for", LoginWindow)
	}
}

// ChangePassword replaces the viewer's own password after checking the
// current one, which counts against the same limit as logins (a stolen
// session must not become a way to guess the password). The viewer's other
// sessions end; their agents stay connected, since it is the owner who
// changes it.
func (auth *Auth) ChangePassword(r *http.Request, viewer store.User, current, password, confirm string) (Problems, error) {
	client := access.ClientIP(r, auth.trustedProxies)
	now := auth.now()

	if auth.limiter.blocked(client, now) {
		return Problems{"current_password": TooManyAttempts}, nil
	}

	hash, err := auth.store.UserPassword(r.Context(), viewer.ID)

	if err != nil {
		return nil, err
	}

	problems := Problems{}

	if !checkPassword(hash, current) {
		auth.failed(client, now)

		problems["current_password"] = "The current password is wrong."
	}

	checkNewPassword(problems, current, password, confirm)

	if len(problems) > 0 {
		return problems, nil
	}

	return nil, setPassword(r.Context(), auth.store, viewer.ID, password, store.PasswordChange{KeepSession: SessionHash(r)})
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
	expires := now.Add(sessionLifetime)

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
