package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOAuthClientCapacityAndCleanup(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	// A full table of clients that never connected, client "1" the oldest.
	_, err = database.DB.ExecContext(ctx, `WITH RECURSIVE clients(id) AS (SELECT 1 UNION ALL SELECT id+1 FROM clients WHERE id < ?)
 INSERT INTO oauth_clients (client_id,name,redirect_uris,created_at) SELECT CAST(id AS TEXT),'Client','[]',strftime('%Y-%m-%dT%H:%M:%fZ', ?, '+' || id || ' seconds') FROM clients`,
		maxOAuthClients, now.Format(time.RFC3339))

	if err != nil {
		t.Fatal(err)
	}

	count := func() int {
		var clients int

		if err := database.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&clients); err != nil {
			t.Fatal(err)
		}

		return clients
	}

	user, err := database.CreateUser(ctx, User{Username: "admin", Role: RoleSuperadmin, CreatedAt: now}, "x")

	if err != nil {
		t.Fatal(err)
	}

	// Client "2" is being authorized: a live code keeps it.
	if err := database.CreateOAuthCode(ctx, "pending-code", OAuthCode{UserID: user, ClientID: "2", RedirectURI: "https://client.example/callback", CodeChallenge: "challenge", ExpiresAt: now.Add(26 * time.Hour)}, now); err != nil {
		t.Fatal(err)
	}

	// Full: a flood of registrations cannot keep a real agent out, the
	// oldest unused client makes room.
	client := OAuthClient{ID: "new-client", Name: "New client", RedirectURIs: []string{"https://client.example/callback"}, CreatedAt: now.Add(time.Minute)}

	if err := database.RegisterOAuthClient(ctx, client); err != nil {
		t.Fatalf("full table refused a registration: %v", err)
	}

	if _, err := database.GetOAuthClient(ctx, "1"); !errors.Is(err, ErrNotFound) || count() != maxOAuthClients {
		t.Fatalf("oldest unused client kept (%v), %d clients", err, count())
	}

	// After unusedClientAge the unused ones go; the one being authorized stays.
	client = OAuthClient{ID: "later-client", Name: "Later client", RedirectURIs: []string{"https://client.example/callback"}, CreatedAt: now.Add(unusedClientAge + time.Hour)}

	if err := database.RegisterOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}

	if _, err := database.GetOAuthClient(ctx, "2"); err != nil {
		t.Fatal("pending authorization lost its client", err)
	}

	if clients := count(); clients != 2 {
		t.Fatalf("unused clients not cleaned up: %d", clients)
	}

	// Every client in use: the table is full for real.
	_, err = database.DB.ExecContext(ctx, `WITH RECURSIVE clients(id) AS (SELECT 1 UNION ALL SELECT id+1 FROM clients WHERE id < ?)
 INSERT INTO oauth_clients (client_id,name,redirect_uris,created_at) SELECT 'used-' || id,'Client','[]',? FROM clients`, maxOAuthClients, formatTime(client.CreatedAt))

	if err != nil {
		t.Fatal(err)
	}

	_, err = database.DB.ExecContext(ctx, `INSERT INTO oauth_codes (code_hash, user_id, client_id, redirect_uri, code_challenge, expires_at)
		SELECT 'code-' || client_id, ?, client_id, 'https://client.example/callback', 'challenge', ? FROM oauth_clients`, user, formatTime(client.CreatedAt.Add(time.Hour)))

	if err != nil {
		t.Fatal(err)
	}

	client = OAuthClient{ID: "one-more", Name: "One more", RedirectURIs: []string{"https://client.example/callback"}, CreatedAt: client.CreatedAt}

	if err := database.RegisterOAuthClient(ctx, client); !errors.Is(err, ErrOAuthCapacity) {
		t.Fatalf("capacity with every client in use: %v", err)
	}
}

func TestOAuthSecurityMigrationPreservesGrants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := openWithMigrations(ctx, path, migrations[:15])

	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	// A personal token from before users existed.
	_, err = database.DB.ExecContext(ctx, `INSERT INTO agent_grants (kind, name, access_hash, created_at) VALUES (?, 'Personal token', 'personal-hash', ?)`,
		GrantToken, formatTime(now))

	if err != nil {
		t.Fatal(err)
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = openWithMigrations(ctx, path, migrations)

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	// Until it has a user, the token does not work; the superadmin adopts it.
	if _, err := database.UseGrant(ctx, "personal-hash", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a token without a user works", err)
	}

	superadmin, err := database.CreateUser(ctx, User{Username: "admin", Role: RoleSuperadmin, CreatedAt: now}, "x")

	if err != nil {
		t.Fatal(err)
	}

	if err := database.AdoptAgents(ctx, superadmin); err != nil {
		t.Fatal(err)
	}

	if grant, err := database.UseGrant(ctx, "personal-hash", now); err != nil || grant.UserID != superadmin || grant.UserName != "admin" {
		t.Fatal("upgrade invalidated existing token", grant, err)
	}

	if _, err := database.DB.ExecContext(ctx, `SELECT * FROM oauth_spent_refresh_tokens`); err != nil {
		t.Fatal("missing replay-protection table", err)
	}
}
