package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

func TestOAuthClientCapacityAndCleanup(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	// A full table of clients that never connected, client "1" the oldest.
	addClients := func(prefix string, at func(index int) time.Time) {
		t.Helper()

		err := database.db.Update(true, func(batch *kv.Batch) error {
			for index := 1; index <= maxOAuthClients; index++ {
				if err := database.oauthClients.Insert(batch, model.OAuthClient{ID: fmt.Sprintf("%s%d", prefix, index), Name: "Client", CreatedAt: at(index)}); err != nil {
					return err
				}
			}

			return nil
		})

		if err != nil {
			t.Fatal(err)
		}
	}

	count := func() int {
		clients, err := database.oauthClients.Primary().All(database.db)

		if err != nil {
			t.Fatal(err)
		}

		return len(clients)
	}

	addClients("", func(index int) time.Time { return now.Add(time.Duration(index) * time.Second) })

	user, err := database.CreateUser(ctx, model.User{Username: "admin", Role: model.RoleSuperadmin, CreatedAt: now}, "x")

	if err != nil {
		t.Fatal(err)
	}

	// Client "2" is being authorized: a live code keeps it.
	if err := database.CreateOAuthCode(ctx, "pending-code", model.OAuthCode{UserID: user, ClientID: "2", ExpiresAt: now.Add(26 * time.Hour)}, now); err != nil {
		t.Fatal(err)
	}

	// Full: the oldest unused client makes room for a new one.
	if err := database.RegisterOAuthClient(ctx, model.OAuthClient{ID: "new-client", CreatedAt: now.Add(time.Minute)}); err != nil {
		t.Fatalf("full table refused a registration: %v", err)
	}

	if _, err := database.GetOAuthClient(ctx, "1"); !errors.Is(err, model.ErrNotFound) || count() != maxOAuthClients {
		t.Fatalf("oldest unused client kept (%v), %d clients", err, count())
	}

	// After unusedClientAge the unused ones go; the one being authorized stays.
	later := now.Add(unusedClientAge + time.Hour)

	if err := database.RegisterOAuthClient(ctx, model.OAuthClient{ID: "later-client", CreatedAt: later}); err != nil {
		t.Fatal(err)
	}

	if _, err := database.GetOAuthClient(ctx, "2"); err != nil {
		t.Fatal("pending authorization lost its client", err)
	}

	if clients := count(); clients != 2 {
		t.Fatalf("unused clients not cleaned up: %d", clients)
	}

	// Every client in use: the table is full for real.
	addClients("used-", func(int) time.Time { return later })

	for index := 1; index <= maxOAuthClients; index++ {
		code := model.OAuthCode{UserID: user, ClientID: fmt.Sprintf("used-%d", index), ExpiresAt: later.Add(time.Hour)}

		if err := database.CreateOAuthCode(ctx, fmt.Sprintf("code-%d", index), code, later); err != nil {
			t.Fatal(err)
		}
	}

	if err := database.RegisterOAuthClient(ctx, model.OAuthClient{ID: "one-more", CreatedAt: later}); !errors.Is(err, model.ErrOAuthCapacity) {
		t.Fatalf("capacity with every client in use: %v", err)
	}
}

// Usernames are unique in any case, there is one superadmin, and deleting a
// user ends their sessions and disconnects their agents.
func TestUserRules(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	root, err := database.CreateUser(ctx, model.User{Username: "Root", Role: model.RoleSuperadmin, CreatedAt: now}, "hash")

	if err != nil {
		t.Fatal(err)
	}

	if _, err := database.CreateUser(ctx, model.User{Username: "root", Role: model.RoleUser, CreatedAt: now}, "hash"); !errors.Is(err, model.ErrUsernameTaken) {
		t.Fatalf("same username in another case: %v", err)
	}

	if _, err := database.CreateUser(ctx, model.User{Username: "second", Role: model.RoleSuperadmin, CreatedAt: now}, "hash"); err == nil {
		t.Fatal("a second superadmin was created")
	}

	user, err := database.CreateUser(ctx, model.User{Username: "dev", Role: model.RoleUser, CreatedAt: now}, "hash")

	if err != nil {
		t.Fatal(err)
	}

	if found, err := database.FindUser(ctx, "DEV"); err != nil || found.ID != user {
		t.Fatalf("find in another case: %+v, %v", found, err)
	}

	for _, hash := range []string{"session-a", "session-b"} {
		if err := database.CreateSession(ctx, hash, user, now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	if err := database.ReplacePersonalToken(ctx, user, "token", now); err != nil {
		t.Fatal(err)
	}

	// Changing the own password keeps this session and the agents.
	if err := database.SetPassword(ctx, user, model.PasswordChange{Hash: "new", KeepSession: "session-a"}); err != nil {
		t.Fatal(err)
	}

	if _, err := database.SessionUser(ctx, "session-b", now); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("the other session survived a password change")
	}

	if has, _ := database.HasPersonalToken(ctx, user); !has {
		t.Fatal("the own password change disconnected the agents")
	}

	if err := database.DeleteUser(ctx, user); err != nil {
		t.Fatal(err)
	}

	if _, err := database.SessionUser(ctx, "session-a", now); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("a session of a deleted user still works")
	}

	if _, err := database.UseGrant(ctx, "token", now); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("a token of a deleted user still works")
	}

	if admin, err := database.Superadmin(ctx); err != nil || admin.ID != root {
		t.Fatalf("superadmin: %+v, %v", admin, err)
	}
}

// A refresh token works once; presented again, it revokes its grant.
func TestRefreshTokenReplayRevokesTheGrant(t *testing.T) {
	ctx := context.Background()
	database := openTest(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	user, _ := database.CreateUser(ctx, model.User{Username: "admin", Role: model.RoleSuperadmin, CreatedAt: now}, "x")
	tokens := model.GrantTokens{AccessHash: "access-1", AccessExpiresAt: now.Add(time.Hour), RefreshHash: "refresh-1", RefreshExpiresAt: now.Add(24 * time.Hour)}
	grant, err := database.CreateGrant(ctx, model.Grant{UserID: user, Kind: model.GrantOAuth, ClientID: "client", CreatedAt: now}, tokens)

	if err != nil {
		t.Fatal(err)
	}

	next := model.GrantTokens{AccessHash: "access-2", AccessExpiresAt: now.Add(time.Hour), RefreshHash: "refresh-2", RefreshExpiresAt: now.Add(24 * time.Hour)}

	if err := database.RotateGrant(ctx, "other-client", "refresh-1", next, now); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("another client rotated the grant: %v", err)
	}

	if err := database.RotateGrant(ctx, "client", "refresh-1", next, now); err != nil {
		t.Fatal(err)
	}

	if _, err := database.UseGrant(ctx, "access-2", now); err != nil {
		t.Fatal("the new access token does not work", err)
	}

	if err := database.RotateGrant(ctx, "client", "refresh-1", next, now); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("a spent refresh token was accepted: %v", err)
	}

	if _, err := database.GetGrant(ctx, grant); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("a replayed refresh token did not revoke the grant")
	}
}
