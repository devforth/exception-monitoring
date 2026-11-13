package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	ExpiresAt     time.Time
}

// Grant is a connected agent or the personal token, without its secrets.
type Grant struct {
	ID         int64
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

// unusedClientAge: clients that registered but never connected are dropped
// after this; registration is open, so they must not pile up.
const unusedClientAge = 24 * time.Hour

const maxOAuthClients = 1024

var ErrOAuthCapacity = errors.New("too many registered OAuth clients")

// RegisterOAuthClient stores a client, first dropping clients that never
// connected within unusedClientAge.
func (store *Store) RegisterOAuthClient(ctx context.Context, client OAuthClient) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	if _, err := transaction.ExecContext(ctx, `DELETE FROM agent_grants WHERE kind = ? AND refresh_expires_at <= ?`, GrantOAuth, formatTime(client.CreatedAt)); err != nil {
		return err
	}

	_, err = transaction.ExecContext(ctx, `DELETE FROM oauth_clients WHERE created_at < ?
		AND client_id NOT IN (SELECT client_id FROM agent_grants)
		AND client_id NOT IN (SELECT client_id FROM oauth_codes WHERE expires_at > ?)`,
		formatTime(client.CreatedAt.Add(-unusedClientAge)), formatTime(client.CreatedAt))

	if err != nil {
		return err
	}

	var count int

	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&count); err != nil {
		return err
	}

	if count >= maxOAuthClients {
		return ErrOAuthCapacity
	}

	uris, err := json.Marshal(client.RedirectURIs)

	if err != nil {
		return err
	}

	_, err = transaction.ExecContext(ctx, `INSERT INTO oauth_clients (client_id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		client.ID, client.Name, string(uris), formatTime(client.CreatedAt))

	if err != nil {
		return err
	}

	return transaction.Commit()
}

// GetOAuthClient reads a client; ErrNotFound when it is not registered.
func (store *Store) GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, error) {
	var (
		client    OAuthClient
		uris      string
		createdAt string
	)

	err := store.DB.QueryRowContext(ctx, `SELECT client_id, name, redirect_uris, created_at FROM oauth_clients WHERE client_id = ?`, clientID).
		Scan(&client.ID, &client.Name, &uris, &createdAt)

	if errors.Is(err, sql.ErrNoRows) {
		return client, ErrNotFound
	}

	if err != nil {
		return client, err
	}

	client.CreatedAt, _ = time.Parse(timeFormat, createdAt)

	return client, json.Unmarshal([]byte(uris), &client.RedirectURIs)
}

// CreateOAuthCode stores a code by its hash, dropping expired ones.
func (store *Store) CreateOAuthCode(ctx context.Context, codeHash string, code OAuthCode, now time.Time) error {
	if _, err := store.DB.ExecContext(ctx, `DELETE FROM oauth_codes WHERE expires_at <= ?`, formatTime(now)); err != nil {
		return err
	}

	_, err := store.DB.ExecContext(ctx, `INSERT INTO oauth_codes (code_hash, client_id, redirect_uri, code_challenge, expires_at) VALUES (?, ?, ?, ?, ?)`,
		codeHash, code.ClientID, code.RedirectURI, code.CodeChallenge, formatTime(code.ExpiresAt))

	return err
}

// TakeOAuthCode deletes a code and returns it: a code works once.
// ErrNotFound when it does not exist or has expired.
func (store *Store) TakeOAuthCode(ctx context.Context, codeHash string, now time.Time) (OAuthCode, error) {
	var (
		code      OAuthCode
		expiresAt string
	)

	err := store.DB.QueryRowContext(ctx, `DELETE FROM oauth_codes WHERE code_hash = ? RETURNING client_id, redirect_uri, code_challenge, expires_at`, codeHash).
		Scan(&code.ClientID, &code.RedirectURI, &code.CodeChallenge, &expiresAt)

	if errors.Is(err, sql.ErrNoRows) {
		return code, ErrNotFound
	}

	if err != nil {
		return code, err
	}

	code.ExpiresAt, _ = time.Parse(timeFormat, expiresAt)

	if !code.ExpiresAt.After(now) {
		return code, ErrNotFound
	}

	return code, nil
}

// CreateGrant stores a grant with its tokens.
func (store *Store) CreateGrant(ctx context.Context, grant Grant, tokens GrantTokens) (int64, error) {
	result, err := store.DB.ExecContext(ctx, `INSERT INTO agent_grants
		(kind, client_id, name, access_hash, access_expires_at, refresh_hash, refresh_expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		grant.Kind, grant.ClientID, grant.Name, tokens.AccessHash, optionalTime(tokens.AccessExpiresAt),
		optionalText(tokens.RefreshHash), optionalTime(tokens.RefreshExpiresAt), formatTime(grant.CreatedAt))

	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// ReplacePersonalToken deletes the personal token, if any, and stores a new one.
func (store *Store) ReplacePersonalToken(ctx context.Context, accessHash string, now time.Time) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	if _, err := transaction.ExecContext(ctx, `DELETE FROM agent_grants WHERE kind = ?`, GrantToken); err != nil {
		return err
	}

	_, err = transaction.ExecContext(ctx, `INSERT INTO agent_grants (kind, name, access_hash, created_at) VALUES (?, 'Personal token', ?, ?)`,
		GrantToken, accessHash, formatTime(now))

	if err != nil {
		return err
	}

	return transaction.Commit()
}

// RotateGrant swaps the tokens of the grant whose refresh token is
// refreshHash, for clientID. ErrNotFound when no such grant exists or its
// refresh token has expired. Reusing a spent token revokes the entire grant.
func (store *Store) RotateGrant(ctx context.Context, clientID, refreshHash string, tokens GrantTokens, now time.Time) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	if _, err := transaction.ExecContext(ctx, `DELETE FROM oauth_spent_refresh_tokens WHERE expires_at <= ?`, formatTime(now)); err != nil {
		return err
	}

	var (
		grantID   int64
		expiresAt string
	)

	err = transaction.QueryRowContext(ctx, `SELECT id, refresh_expires_at FROM agent_grants
		WHERE kind = ? AND client_id = ? AND refresh_hash = ? AND refresh_expires_at > ?`,
		GrantOAuth, clientID, refreshHash, formatTime(now)).Scan(&grantID, &expiresAt)

	if errors.Is(err, sql.ErrNoRows) {
		// A matching spent token proves replay. Bind the lookup to client_id
		// so another public client cannot revoke somebody else's grant.
		_, err = transaction.ExecContext(ctx, `DELETE FROM oauth_codes WHERE client_id = ? AND EXISTS
			(SELECT 1 FROM oauth_spent_refresh_tokens spent JOIN agent_grants grants ON grants.id = spent.grant_id
			WHERE spent.refresh_hash = ? AND grants.client_id = ?)`, clientID, refreshHash, clientID)

		if err != nil {
			return err
		}

		_, err = transaction.ExecContext(ctx, `DELETE FROM agent_grants WHERE client_id = ? AND id IN
			(SELECT grant_id FROM oauth_spent_refresh_tokens WHERE refresh_hash = ?)`, clientID, refreshHash)

		if err != nil {
			return err
		}

		if err := transaction.Commit(); err != nil {
			return err
		}

		return ErrNotFound
	}

	if err != nil {
		return err
	}

	if _, err := transaction.ExecContext(ctx, `INSERT INTO oauth_spent_refresh_tokens (refresh_hash, grant_id, expires_at) VALUES (?, ?, ?)`, refreshHash, grantID, expiresAt); err != nil {
		return err
	}

	_, err = transaction.ExecContext(ctx, `UPDATE agent_grants
		SET access_hash = ?, access_expires_at = ?, refresh_hash = ?, refresh_expires_at = ?
		WHERE id = ?`,
		tokens.AccessHash, optionalTime(tokens.AccessExpiresAt), tokens.RefreshHash, optionalTime(tokens.RefreshExpiresAt),
		grantID)

	if err != nil {
		return err
	}

	return transaction.Commit()
}

// lastUsedStep: last_used_at is written at most this often, not on every call.
const lastUsedStep = time.Minute

// UseGrant finds the grant of a valid access token and records the use.
// ErrNotFound when the token is unknown or expired.
func (store *Store) UseGrant(ctx context.Context, accessHash string, now time.Time) (Grant, error) {
	var (
		grant     Grant
		createdAt string
		lastUsed  sql.NullString
	)

	err := store.DB.QueryRowContext(ctx, `SELECT id, kind, client_id, name, created_at, last_used_at FROM agent_grants
		WHERE access_hash = ? AND (access_expires_at IS NULL OR access_expires_at > ?)`, accessHash, formatTime(now)).
		Scan(&grant.ID, &grant.Kind, &grant.ClientID, &grant.Name, &createdAt, &lastUsed)

	if errors.Is(err, sql.ErrNoRows) {
		return grant, ErrNotFound
	}

	if err != nil {
		return grant, err
	}

	grant.CreatedAt, _ = time.Parse(timeFormat, createdAt)
	_, err = store.DB.ExecContext(ctx, `UPDATE agent_grants SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		formatTime(now), grant.ID, formatTime(now.Add(-lastUsedStep)))

	return grant, err
}

// ListGrants lists the connected agents and the personal token, newest
// first, dropping agents whose refresh token has expired (they would have
// to connect again anyway).
func (store *Store) ListGrants(ctx context.Context, now time.Time) ([]Grant, error) {
	if _, err := store.DB.ExecContext(ctx, `DELETE FROM agent_grants WHERE kind = ? AND refresh_expires_at <= ?`, GrantOAuth, formatTime(now)); err != nil {
		return nil, err
	}

	rows, err := store.DB.QueryContext(ctx, `SELECT id, kind, client_id, name, created_at, last_used_at FROM agent_grants ORDER BY id DESC`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var grants []Grant

	for rows.Next() {
		var (
			grant     Grant
			createdAt string
			lastUsed  sql.NullString
		)

		if err := rows.Scan(&grant.ID, &grant.Kind, &grant.ClientID, &grant.Name, &createdAt, &lastUsed); err != nil {
			return nil, err
		}

		grant.CreatedAt, _ = time.Parse(timeFormat, createdAt)

		if lastUsed.Valid {
			used, _ := time.Parse(timeFormat, lastUsed.String)
			grant.LastUsedAt = &used
		}

		grants = append(grants, grant)
	}

	return grants, rows.Err()
}

// DeleteGrant disconnects an agent: its tokens stop working at once.
func (store *Store) DeleteGrant(ctx context.Context, id int64) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	// Also invalidate authorization codes approved before disconnecting.
	if _, err := transaction.ExecContext(ctx, `DELETE FROM oauth_codes WHERE client_id IN
		(SELECT client_id FROM agent_grants WHERE id = ? AND kind = ?)`, id, GrantOAuth); err != nil {
		return err
	}

	result, err := transaction.ExecContext(ctx, `DELETE FROM agent_grants WHERE id = ?`, id)

	if err != nil {
		return err
	}

	if rows, err := result.RowsAffected(); err != nil || rows == 0 {
		return errors.Join(err, ErrNotFound)
	}

	return transaction.Commit()
}

func optionalTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}

	return formatTime(value)
}

func optionalText(value string) any {
	if value == "" {
		return nil
	}

	return value
}
