package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestEmailDestinationVerification(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()

	id, err := database.CreateDestination(ctx, Destination{Type: "email", Address: "Ops@Example.com"}, baseTime)

	if err != nil {
		t.Fatal(err)
	}

	// Addresses are stored lowercase and matched case-insensitively.
	found, err := database.FindEmailDestination(ctx, "ops@example.COM")

	if err != nil || found.ID != id || found.Verified || found.Address != "ops@example.com" {
		t.Fatalf("find: %+v %v", found, err)
	}

	check := func(hash string, at time.Time) VerifyResult {
		t.Helper()

		result, err := database.CheckVerificationCode(ctx, id, hash, at)

		if err != nil {
			t.Fatal(err)
		}

		return result
	}

	if result := check("anything", baseTime); result != VerifyExpired {
		t.Fatalf("no code sent yet: %v", result)
	}

	if err := database.SetVerificationCode(ctx, id, "hash-1", baseTime.Add(15*time.Minute), baseTime); err != nil {
		t.Fatal(err)
	}

	if destination, _ := database.GetDestination(ctx, id); !destination.VerifySentAt.Equal(baseTime) {
		t.Fatalf("verify_sent_at = %v", destination.VerifySentAt)
	}

	if result := check("hash-1", baseTime.Add(16*time.Minute)); result != VerifyExpired {
		t.Fatalf("after expiry: %v", result)
	}

	for range MaxVerifyAttempts {
		if result := check("wrong", baseTime.Add(time.Minute)); result != VerifyWrong {
			t.Fatalf("wrong code: %v", result)
		}
	}

	// Even the right code is refused after too many wrong ones.
	if result := check("hash-1", baseTime.Add(time.Minute)); result != VerifyTooManyTries {
		t.Fatalf("after %d wrong codes: %v", MaxVerifyAttempts, result)
	}

	// A new code resets the attempts.
	if err := database.SetVerificationCode(ctx, id, "hash-2", baseTime.Add(time.Hour), baseTime); err != nil {
		t.Fatal(err)
	}

	confirmedAt := baseTime.Add(time.Minute)

	if result := check("hash-2", confirmedAt); result != VerifyOK {
		t.Fatalf("right code: %v", result)
	}

	destination, _ := database.GetDestination(ctx, id)

	if !destination.Verified || !destination.VerifiedAt.Equal(confirmedAt) {
		t.Fatalf("after confirming: %+v", destination)
	}

	// The code is used up.
	if result := check("hash-2", baseTime.Add(2*time.Minute)); result != VerifyExpired {
		t.Fatalf("code reused: %v", result)
	}
}

func TestConnectionLifecycle(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()

	unconfirmed, _ := database.CreateDestination(ctx, Destination{Type: "email", Address: "new@example.com"}, baseTime)

	// A connection needs a confirmed destination.
	if _, err := database.CreateConnection(ctx, Connection{Name: "x", DestinationID: unconfirmed, AllProjects: true, Levels: []string{"error"}}, baseTime); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("connection to an unconfirmed address: %v", err)
	}

	destination, _ := database.CreateDestination(ctx, Destination{Type: "email", Address: "team@example.com", Verified: true, VerifiedAt: baseTime}, baseTime)

	id, err := database.CreateConnection(ctx, Connection{
		Name: "Backend team", DestinationID: destination, ProjectIDs: []int{2, 1, 2}, Levels: []string{"error", "fatal"}, Active: true,
	}, baseTime)

	if err != nil {
		t.Fatal(err)
	}

	connection, err := database.GetConnection(ctx, id)

	if err != nil {
		t.Fatal(err)
	}

	// The destination fields come with the connection.
	if connection.Name != "Backend team" || connection.Type != "email" || connection.Address != "team@example.com" || !connection.Verified ||
		fmt.Sprint(connection.ProjectIDs) != "[1 2]" || fmt.Sprint(connection.Levels) != "[error fatal]" || !connection.Active {
		t.Fatalf("created: %+v", connection)
	}

	// "All projects" drops the explicit list: it covers projects added later.
	connection.AllProjects = true
	connection.Name = "Everyone"

	if err := database.UpdateConnection(ctx, connection, baseTime); err != nil {
		t.Fatal(err)
	}

	listed, err := database.ListConnections(ctx)

	if err != nil || len(listed) != 1 || !listed[0].AllProjects || len(listed[0].ProjectIDs) != 0 || listed[0].Name != "Everyone" {
		t.Fatalf("listed: %+v %v", listed, err)
	}

	if err := database.EnqueueOutbox(ctx, []string{fmt.Sprint(id)}, "notification", []byte(`{}`), nil, baseTime); err != nil {
		t.Fatal(err)
	}

	if err := database.DeleteConnection(ctx, id); err != nil {
		t.Fatal(err)
	}

	if _, err := database.GetConnection(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}

	// Its queued messages go with it, the confirmed address stays.
	if counts, _ := database.OutboxCounts(ctx); len(counts) != 0 {
		t.Fatalf("outbox after delete: %v", counts)
	}

	if kept, err := database.FindEmailDestination(ctx, "team@example.com"); err != nil || !kept.Verified {
		t.Fatalf("the confirmed address must survive the connection: %+v %v", kept, err)
	}
}

// Migration 7 moves the destination fields of existing connections into
// notification_destinations and keeps their projects.
func TestDestinationMigrationKeepsConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	before, err := openWithMigrations(ctx, path, migrations[:6])

	if err != nil {
		t.Fatal(err)
	}

	_, err = before.DB.ExecContext(ctx, `
		INSERT INTO notification_connections (id, name, type, address, secret, all_projects, levels, active, verified, created_at, updated_at)
		VALUES (4, 'Ops', 'email', 'Ops@Example.com', '', 0, 'error', 1, 1, 'x', '2026-11-12T10:00:00.000000Z'),
		       (5, 'Chat', 'slack', '', 'encrypted', 1, 'fatal', 0, 1, 'x', 'x');
		INSERT INTO notification_connection_projects VALUES (4, 1), (4, 2);`)

	if err != nil {
		t.Fatal(err)
	}

	closeDatabase(t, before)

	database, err := openWithMigrations(ctx, path, migrations)

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	connections, err := database.ListConnections(ctx)

	if err != nil || len(connections) != 2 {
		t.Fatalf("connections after migration: %+v %v", connections, err)
	}

	ops, chat := connections[0], connections[1]

	if ops.ID != 4 || ops.Address != "ops@example.com" || !ops.Verified || !ops.Active || fmt.Sprint(ops.ProjectIDs) != "[1 2]" {
		t.Errorf("email connection: %+v", ops)
	}

	if chat.ID != 5 || chat.Type != "slack" || chat.Secret != "encrypted" || !chat.AllProjects || chat.Active {
		t.Errorf("chat connection: %+v", chat)
	}

	if destination, _ := database.GetDestination(ctx, ops.DestinationID); destination.VerifiedAt.IsZero() {
		t.Error("a confirmed address gets a confirmation date")
	}
}
