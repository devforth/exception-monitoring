package store

import (
	"errors"
	"time"
)

// Grant kinds: an agent connected with OAuth, or the personal token.
const (
	GrantOAuth = "oauth"
	GrantToken = "token"
)

// OAuthClient is an agent registered for OAuth (RFC 7591).
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// OAuthCode is an authorization code waiting to be traded for tokens.
type OAuthCode struct {
	UserID        string // who allowed the agent
	ClientID      string
	ClientName    string
	RedirectURI   string
	CodeChallenge string
	ExpiresAt     time.Time
}

// Grant is a connected agent or a personal token, without its secrets. It
// acts for UserID.
type Grant struct {
	ID         string
	UserID     string
	UserName   string // the user's display name, read with the grant
	Kind       string
	ClientID   string
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// GrantTokens are the hashes and expiry of a grant's tokens. A zero expiry
// means none (the personal token).
type GrantTokens struct {
	AccessHash       string
	AccessExpiresAt  time.Time
	RefreshHash      string
	RefreshExpiresAt time.Time
}

var ErrOAuthCapacity = errors.New("too many registered OAuth clients")
