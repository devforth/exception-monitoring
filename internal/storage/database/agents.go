package database

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

const (
	// unusedClientAge: clients that registered but never connected go after
	// this; registration is open, so they must not pile up.
	unusedClientAge = time.Hour
	maxOAuthClients = 1024
	// lastUsedStep: a grant's last use is written at most this often.
	lastUsedStep = time.Minute
)

// codeRecord is an authorization code by its hash.
type codeRecord struct {
	model.OAuthCode
	Hash string `json:"hash"`
}

// grantRecord is a grant with the hashes of its tokens; UserName is read from the user.
type grantRecord struct {
	model.Grant
	model.GrantTokens
}

// spentToken is a refresh token already exchanged, kept until it would have expired.
type spentToken struct {
	Hash      string    `json:"hash"`
	GrantID   string    `json:"grant_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RegisterOAuthClient stores a client, first dropping clients that never
// connected within unusedClientAge; when full, the oldest unused make room.
func (database *Store) RegisterOAuthClient(_ context.Context, client model.OAuthClient) error {
	now := client.CreatedAt

	return database.db.Update(true, func(batch *kv.Batch) error {
		if err := database.dropExpiredGrants(batch, now); err != nil {
			return err
		}

		clients, err := database.oauthClients.Primary().All(batch)

		if err != nil {
			return err
		}

		// Oldest first: when the table is full, the oldest unused make room.
		slices.SortFunc(clients, func(a, b model.OAuthClient) int { return a.CreatedAt.Compare(b.CreatedAt) })

		count := len(clients)

		for _, stored := range clients {
			used, err := database.clientUsed(batch, stored.ID, now)

			if err != nil {
				return err
			}

			if used || (!stored.CreatedAt.Before(now.Add(-unusedClientAge)) && count < maxOAuthClients) {
				continue
			}

			if err := database.oauthClients.Delete(batch, stored); err != nil {
				return err
			}

			count--
		}

		if count >= maxOAuthClients {
			return model.ErrOAuthCapacity
		}

		return database.oauthClients.Insert(batch, client)
	})
}

// clientUsed: the client has a grant or a code that has not expired.
func (database *Store) clientUsed(reader kv.Reader, clientID string, now time.Time) (bool, error) {
	if _, err := database.grants.By("client", kv.Text(clientID)).First(reader); !errors.Is(err, kv.ErrNotFound) {
		return err == nil, err
	}

	used := false

	err := database.oauthCodes.By("client", kv.Text(clientID)).Each(reader, func(entry kv.Entry[codeRecord]) (bool, error) {
		used = entry.Record.ExpiresAt.After(now)

		return !used, nil
	})

	return used, err
}

// GetOAuthClient reads a client; ErrNotFound when it is not registered.
func (database *Store) GetOAuthClient(_ context.Context, clientID string) (model.OAuthClient, error) {
	client, err := database.oauthClients.Get(database.db, kv.K(kv.Text(clientID)))

	return client, notFound(err)
}

// CreateOAuthCode stores a code by its hash, dropping expired ones.
func (database *Store) CreateOAuthCode(_ context.Context, codeHash string, code model.OAuthCode, now time.Time) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		err := database.oauthCodes.Primary().Where(func(stored codeRecord) bool { return !stored.ExpiresAt.After(now) }).
			Each(batch, func(entry kv.Entry[codeRecord]) (bool, error) {
				database.oauthCodes.DeleteEntry(batch, entry)

				return true, nil
			})

		if err != nil {
			return err
		}

		return database.oauthCodes.Insert(batch, codeRecord{OAuthCode: code, Hash: codeHash})
	})
}

// TakeOAuthCode deletes a code and returns it: a code works once. ErrNotFound
// when it does not exist or has expired.
func (database *Store) TakeOAuthCode(_ context.Context, codeHash string, now time.Time) (model.OAuthCode, error) {
	var code codeRecord

	err := database.db.Update(true, func(batch *kv.Batch) error {
		var err error

		if code, err = database.oauthCodes.Get(batch, kv.K(kv.Text(codeHash))); err != nil {
			return notFound(err)
		}

		return database.oauthCodes.Delete(batch, code)
	})

	if err == nil && !code.ExpiresAt.After(now) {
		err = model.ErrNotFound
	}

	return code.OAuthCode, err
}

// CreateGrant stores a grant with its tokens.
func (database *Store) CreateGrant(_ context.Context, grant model.Grant, tokens model.GrantTokens) (string, error) {
	err := database.db.Update(true, func(batch *kv.Batch) error {
		var err error

		if grant.ID, err = freeID(batch, database.grants); err != nil {
			return err
		}

		return database.grants.Insert(batch, grantRecord{Grant: grant, GrantTokens: tokens})
	})

	return grant.ID, err
}

// ReplacePersonalToken replaces the user's personal token: one per user.
func (database *Store) ReplacePersonalToken(_ context.Context, userID string, accessHash string, now time.Time) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		id, err := freeID(batch, database.grants)

		if err != nil {
			return err
		}

		if old, err := database.grants.GetBy(batch, "token", kv.K(kv.Text(userID))); err == nil {
			if err := database.grants.Delete(batch, old); err != nil {
				return err
			}
		} else if !errors.Is(err, kv.ErrNotFound) {
			return err
		}

		grant := model.Grant{ID: id, UserID: userID, Kind: model.GrantToken, Name: "Personal token", CreatedAt: now.UTC()}

		return database.grants.Insert(batch, grantRecord{Grant: grant, GrantTokens: model.GrantTokens{AccessHash: accessHash}})
	})
}

// RotateGrant swaps the tokens of the grant whose refresh token is
// refreshHash, for clientID. ErrNotFound when there is no such grant or its
// refresh token has expired; a spent token presented again revokes its grant.
func (database *Store) RotateGrant(_ context.Context, clientID, refreshHash string, tokens model.GrantTokens, now time.Time) error {
	revoked := false

	err := database.db.Update(true, func(batch *kv.Batch) error {
		err := database.spentTokens.Primary().Where(func(spent spentToken) bool { return !spent.ExpiresAt.After(now) }).
			Each(batch, func(entry kv.Entry[spentToken]) (bool, error) {
				database.spentTokens.DeleteEntry(batch, entry)

				return true, nil
			})

		if err != nil {
			return err
		}

		grant, err := database.grants.GetBy(batch, "refresh", kv.K(kv.Text(clientID), kv.Text(refreshHash)))

		if err == nil && grant.Kind == model.GrantOAuth && grant.RefreshExpiresAt.After(now) {
			if err := database.spentTokens.Insert(batch, spentToken{Hash: refreshHash, GrantID: grant.ID, ExpiresAt: grant.RefreshExpiresAt}); err != nil {
				return err
			}

			grant.GrantTokens = tokens

			return database.grants.Save(batch, grant)
		}

		if err != nil && !errors.Is(err, kv.ErrNotFound) {
			return err
		}

		// A spent token of this client proves a replay: the grant goes. Bound
		// to the client, so another public client cannot revoke someone's grant.
		revoked = true
		spent, err := database.spentTokens.Get(batch, kv.K(kv.Text(refreshHash)))

		if errors.Is(err, kv.ErrNotFound) {
			return nil
		}

		if err != nil {
			return err
		}

		stolen, err := database.grants.Get(batch, kv.K(kv.Text(spent.GrantID)))

		if errors.Is(err, kv.ErrNotFound) || (err == nil && stolen.ClientID != clientID) {
			return nil
		}

		if err != nil {
			return err
		}

		return database.deleteGrant(batch, stolen)
	})

	if err == nil && revoked {
		err = model.ErrNotFound
	}

	return err
}

// UseGrant finds the grant of a valid access token and records the use.
// ErrNotFound when the token is unknown or expired, or its user is gone.
func (database *Store) UseGrant(_ context.Context, accessHash string, now time.Time) (model.Grant, error) {
	grant, err := database.grants.GetBy(database.db, "access", kv.K(kv.Text(accessHash)))

	if err != nil {
		return model.Grant{}, notFound(err)
	}

	if !grant.AccessExpiresAt.IsZero() && !grant.AccessExpiresAt.After(now) {
		return model.Grant{}, model.ErrNotFound
	}

	view, err := database.grantView(database.db, grant)

	if err != nil || (grant.LastUsedAt != nil && grant.LastUsedAt.After(now.Add(-lastUsedStep))) {
		return view, err
	}

	err = database.db.Update(true, func(batch *kv.Batch) error {
		stored, err := database.grants.Get(batch, kv.K(kv.Text(grant.ID)))

		if err != nil {
			return notFound(err)
		}

		at := now.UTC()
		stored.LastUsedAt = &at

		return database.grants.Save(batch, stored)
	})

	return view, err
}

// grantView is the grant with its user's name; ErrNotFound without the user.
func (database *Store) grantView(reader kv.Reader, grant grantRecord) (model.Grant, error) {
	user, err := database.user(reader, grant.UserID)

	if err != nil {
		return model.Grant{}, err
	}

	view := grant.Grant
	view.UserName = user.DisplayName()

	return view, nil
}

// ListGrants lists the agents and the personal token of a user, newest first,
// dropping agents whose refresh token has expired.
func (database *Store) ListGrants(_ context.Context, userID string, now time.Time) ([]model.Grant, error) {
	user, err := database.user(database.db, userID)

	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var (
		grants  []model.Grant
		expired []kv.Entry[grantRecord]
	)

	err = database.grants.By("user", kv.Text(userID)).Newest().Each(database.db, func(entry kv.Entry[grantRecord]) (bool, error) {
		if refreshExpired(entry.Record, now) {
			expired = append(expired, entry)

			return true, nil
		}

		grant := entry.Record.Grant
		grant.UserName = user.DisplayName()
		grants = append(grants, grant)

		return true, nil
	})

	if err != nil || len(expired) == 0 {
		return grants, err
	}

	// They would have to connect again anyway; written only when there are some.
	return grants, database.db.Update(true, func(batch *kv.Batch) error {
		for _, entry := range expired {
			database.grants.DeleteEntry(batch, entry)
		}

		return nil
	})
}

// refreshExpired: an agent whose refresh token has expired.
func refreshExpired(grant grantRecord, now time.Time) bool {
	return grant.Kind == model.GrantOAuth && !grant.RefreshExpiresAt.IsZero() && !grant.RefreshExpiresAt.After(now)
}

// GetGrant reads a grant (to check whose it is).
func (database *Store) GetGrant(_ context.Context, id string) (model.Grant, error) {
	grant, err := database.grants.Get(database.db, kv.K(kv.Text(id)))

	if err != nil {
		return model.Grant{}, notFound(err)
	}

	return database.grantView(database.db, grant)
}

// DeleteGrant disconnects an agent: its tokens and the codes of its client stop working at once.
func (database *Store) DeleteGrant(_ context.Context, id string) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		grant, err := database.grants.Get(batch, kv.K(kv.Text(id)))

		if err != nil {
			return notFound(err)
		}

		return database.deleteGrant(batch, grant)
	})
}

// HasPersonalToken reports whether the user has a personal token.
func (database *Store) HasPersonalToken(_ context.Context, userID string) (bool, error) {
	return database.grants.Taken(database.db, "token", kv.K(kv.Text(userID)))
}

// deleteGrant deletes a grant and, for an agent, the codes of its client.
func (database *Store) deleteGrant(batch *kv.Batch, grant grantRecord) error {
	if grant.Kind == model.GrantOAuth {
		err := database.oauthCodes.By("client", kv.Text(grant.ClientID)).Each(batch, func(entry kv.Entry[codeRecord]) (bool, error) {
			database.oauthCodes.DeleteEntry(batch, entry)

			return true, nil
		})

		if err != nil {
			return err
		}
	}

	return database.grants.Delete(batch, grant)
}

// deleteAgentsOf disconnects every agent of a user and drops the codes they allowed.
func (database *Store) deleteAgentsOf(batch *kv.Batch, userID string) error {
	err := database.oauthCodes.By("user", kv.Text(userID)).Each(batch, func(entry kv.Entry[codeRecord]) (bool, error) {
		database.oauthCodes.DeleteEntry(batch, entry)

		return true, nil
	})

	if err != nil {
		return err
	}

	return database.grants.By("user", kv.Text(userID)).Each(batch, func(entry kv.Entry[grantRecord]) (bool, error) {
		database.grants.DeleteEntry(batch, entry)

		return true, nil
	})
}

// dropExpiredGrants deletes agents whose refresh token has expired.
func (database *Store) dropExpiredGrants(batch *kv.Batch, now time.Time) error {
	return database.grants.Primary().Where(func(grant grantRecord) bool { return refreshExpired(grant, now) }).Each(batch, func(entry kv.Entry[grantRecord]) (bool, error) {
		database.grants.DeleteEntry(batch, entry)

		return true, nil
	})
}
