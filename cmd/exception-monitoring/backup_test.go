package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/database"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

func openWithIssue(t *testing.T, dataDir string) *database.Store {
	t.Helper()

	db, err := database.Open(dataDir)

	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	_, err = db.SaveEvent(context.Background(), store.NewEvent{
		ProjectID: 1, EventID: strings.Repeat("a", 32), Fingerprint: "backup", Title: "Error: backup",
		Level: "error", Timestamp: now, ReceivedAt: now, Payload: []byte(`{}`),
	})

	if err != nil {
		t.Fatal(err)
	}

	return db
}

// checkBackup opens a backup and finds the issue written before it.
func checkBackup(t *testing.T, target string) {
	t.Helper()

	db, err := database.Open(target)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	if problems, err := db.Verify(); err != nil || len(problems) > 0 {
		t.Errorf("verify: %v %v", problems, err)
	}

	if count, err := db.CountIssues(context.Background(), store.IssueFilter{}); err != nil || count != 1 {
		t.Errorf("backup holds %d issues (%v), want 1", count, err)
	}
}

func TestBackupThroughServer(t *testing.T) {
	dataDir := t.TempDir()
	db := openWithIssue(t, dataDir)

	defer func() { _ = db.Close() }()

	server := listenControl(dataDir, db, slog.New(slog.DiscardHandler))

	if server == nil {
		t.Fatal("no control socket")
	}

	defer func() { _ = server.Close() }()

	target := filepath.Join(t.TempDir(), "online")

	var out bytes.Buffer

	if err := runBackup(context.Background(), dataDir, target, &out); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out.String(), "no server") || !strings.Contains(out.String(), "backup written to "+target) {
		t.Errorf("output %q", out.String())
	}

	checkBackup(t, target)
}

func TestBackupWithoutServer(t *testing.T) {
	dataDir := t.TempDir()
	db := openWithIssue(t, dataDir)
	target := filepath.Join(t.TempDir(), "locked")

	// Open in another process, nobody on the socket.
	err := runBackup(context.Background(), dataDir, target, &bytes.Buffer{})

	if err == nil || !strings.Contains(err.Error(), "open in another process") {
		t.Errorf("locked database: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	target = filepath.Join(t.TempDir(), "offline")

	var out bytes.Buffer

	if err := runBackup(context.Background(), dataDir, target, &out); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "the database is read here") {
		t.Errorf("output %q", out.String())
	}

	checkBackup(t, target)

	// Without a path it goes to DATA_DIR/backups/<time>.
	if err := runBackup(context.Background(), dataDir, "", &out); err != nil {
		t.Fatal(err)
	}

	made, err := filepath.Glob(filepath.Join(dataDir, "backups", "*"))

	if err != nil || len(made) != 1 {
		t.Fatalf("default backups: %v %v", made, err)
	}

	checkBackup(t, made[0])

	if err := runBackup(context.Background(), dataDir, "relative", &out); err == nil {
		t.Error("a relative path was accepted")
	}

	if err := runBackup(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "none"), &out); err == nil {
		t.Error("a backup of a data directory without a database succeeded")
	}
}
