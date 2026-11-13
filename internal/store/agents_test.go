package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOAuthClientCapacityAndCleanup(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	_, err = database.DB.ExecContext(ctx, `WITH RECURSIVE clients(id) AS (SELECT 1 UNION ALL SELECT id+1 FROM clients WHERE id < ?)
 INSERT INTO oauth_clients (client_id,name,redirect_uris,created_at) SELECT CAST(id AS TEXT),'Client','[]',? FROM clients`, maxOAuthClients, formatTime(now))

	if err != nil {
		t.Fatal(err)
	}

	client := OAuthClient{ID: "new-client", Name: "New client", RedirectURIs: []string{"https://client.example/callback"}, CreatedAt: now}

	if err := database.RegisterOAuthClient(ctx, client); !errors.Is(err, ErrOAuthCapacity) {
		t.Fatalf("capacity not enforced: %v", err)
	}
	// Old clients with live authorization codes must survive cleanup.
	if err := database.CreateOAuthCode(ctx, "pending-code", OAuthCode{ClientID: "1", RedirectURI: "https://client.example/callback", CodeChallenge: "challenge", ExpiresAt: now.Add(26 * time.Hour)}, now); err != nil {
		t.Fatal(err)
	}

	client.CreatedAt = now.Add(25 * time.Hour)

	if err := database.RegisterOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}

	if _, err := database.GetOAuthClient(ctx, "1"); err != nil {
		t.Fatal("pending authorization lost its client", err)
	}

	var count int

	if err := database.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&count); err != nil {
		t.Fatal(err)
	}

	if count != 2 {
		t.Fatalf("unused clients not cleaned up: %d", count)
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

	if err := database.ReplacePersonalToken(ctx, "personal-hash", now); err != nil {
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

	if _, err := database.UseGrant(ctx, "personal-hash", now); err != nil {
		t.Fatal("upgrade invalidated existing token", err)
	}

	if _, err := database.DB.ExecContext(ctx, `SELECT * FROM oauth_spent_refresh_tokens`); err != nil {
		t.Fatal("missing replay-protection table", err)
	}
}
