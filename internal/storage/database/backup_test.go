package database

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/secret"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

func openDisk(t *testing.T, dataDir string) *Store {
	t.Helper()

	database, err := Open(dataDir)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return database
}

func TestBackupWhileWriting(t *testing.T) {
	database := openDisk(t, t.TempDir())
	start := time.Now()

	for index := range 50 {
		saveEvent(t, database, "before", start.Add(time.Duration(index)*time.Millisecond))
	}

	// Writes go on while the backup copies.
	stop := make(chan struct{})
	written := atomic.Int64{}

	var writers sync.WaitGroup

	writers.Go(func() {
		for index := 0; ; index++ {
			select {
			case <-stop:
				return
			default:
			}

			at := start.Add(time.Hour + time.Duration(index)*time.Millisecond)
			_, err := database.SaveEvent(context.Background(), model.NewEvent{
				ProjectID: 1, EventID: fmt.Sprintf("%032x", at.UnixNano()), Fingerprint: "during", Title: "Error: during",
				Level: "error", Timestamp: at, ReceivedAt: at, Payload: []byte(`{}`),
			})

			if err != nil {
				t.Error(err)

				return
			}

			written.Add(1)
		}
	})

	// Once copied, the backup waits for more writes while it holds the snapshot.
	defer func(previous int64) { backupProgressBytes = previous }(backupProgressBytes)

	backupProgressBytes = 0

	var atCopy, afterWait int64

	progress := func(int64, int64) {
		if atCopy > 0 {
			return
		}

		atCopy = max(written.Load(), 1)

		for deadline := time.Now().Add(10 * time.Second); written.Load() < atCopy+20 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}

		afterWait = written.Load()
	}

	target := filepath.Join(t.TempDir(), "backups", "first")
	result, err := database.Backup(context.Background(), target, progress)

	close(stop)
	writers.Wait()

	if err != nil {
		t.Fatal(err)
	}

	if result.Keys == 0 || result.SecretKey {
		t.Errorf("result %+v, want keys and no secret key", result)
	}

	checkCollections(t, database)

	copied := openDisk(t, target)
	checkCollections(t, copied)

	if issue := storedIssue(copied, "before"); issue == nil || issue.Stored != 50 {
		t.Errorf("backup holds %+v, want the 50 events written before it", issue)
	}

	if afterWait < atCopy+20 {
		t.Fatalf("%d events written while the backup ran, want 20", afterWait-atCopy)
	}

	if issue := storedIssue(copied, "during"); issue != nil && int64(issue.Stored) >= afterWait {
		t.Errorf("backup holds %d events written during it, %d were written before it ended", issue.Stored, afterWait)
	}

	if result.Size == 0 {
		t.Error("no size on disk")
	}

	// Nothing is left beside the target.
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Errorf("backups directory holds %d entries, want 1", len(entries))
	}
}

func TestBackupTarget(t *testing.T) {
	dataDir := t.TempDir()
	database := openDisk(t, dataDir)
	ctx := context.Background()

	nonEmpty := t.TempDir()
	kept := filepath.Join(nonEmpty, "kept")

	if err := os.WriteFile(kept, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A link to the database leads inside it.
	link := filepath.Join(t.TempDir(), "link")

	if err := os.Symlink(Location(dataDir), link); err != nil {
		t.Fatal(err)
	}

	// target -> the reason given
	refused := map[string]string{
		"backups/first": "is not absolute",
		nonEmpty:        "is not empty",
		nonEmpty + "/":  "is not empty",
		filepath.Join(Location(dataDir), "backup"): "is inside the database",
		filepath.Join(link, "backup"):              "is inside the database",
	}

	for target, reason := range refused {
		if _, err := database.Backup(ctx, target, nil); err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("backup to %s: %v, want %q", target, err, reason)
		}
	}

	if content, err := os.ReadFile(kept); err != nil || string(content) != "kept" {
		t.Errorf("file in the refused target: %q %v", content, err)
	}

	if entries, _ := os.ReadDir(Location(dataDir)); slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return entry.IsDir() }) {
		t.Error("a directory was written inside the database")
	}

	// An empty directory (a mount point too) is kept and filled; a trailing slash changes nothing.
	parent := t.TempDir()
	empty := filepath.Join(parent, "empty")

	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	before, _ := os.Stat(empty)

	for _, target := range []string{empty + "/", filepath.Join(parent, "new") + "/"} {
		if _, err := database.Backup(ctx, target, nil); err != nil {
			t.Fatal(err)
		}

		entries, _ := os.ReadDir(target)

		if len(entries) != 1 || entries[0].Name() != engineName {
			t.Errorf("%s holds %v, want only %s", target, entries, engineName)
		}

		openDisk(t, target)
	}

	if after, err := os.Stat(empty); err != nil || !os.SameFile(before, after) {
		t.Errorf("the empty target was replaced (%v)", err)
	}

	if entries, _ := os.ReadDir(parent); len(entries) != 2 {
		t.Errorf("parent holds %v, want the two backups", entries)
	}
}

// A failed backup leaves neither the target nor its temporary directory.
func TestBackupFailedLeavesNothing(t *testing.T) {
	database := openDisk(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())

	cancel()

	parent := t.TempDir()
	empty := filepath.Join(parent, "empty")

	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{filepath.Join(parent, "new"), empty} {
		if _, err := database.Backup(ctx, target, nil); !errors.Is(err, context.Canceled) {
			t.Errorf("backup to %s: %v, want canceled", target, err)
		}
	}

	if entries, _ := os.ReadDir(parent); len(entries) != 1 || entries[0].Name() != "empty" {
		t.Errorf("parent holds %v, want only the empty target", entries)
	}

	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("the empty target holds %v", entries)
	}
}

// While a backup holds its snapshot, the cleanup by size does not start.
func TestCleanupWaitsForBackup(t *testing.T) {
	database := openDisk(t, t.TempDir())
	ctx := context.Background()

	saveEvent(t, database, "kept", time.Now())
	database.backups.Add(1)

	if run, err := database.Cleanup(ctx, 1); err != nil || run.Status != "" {
		t.Errorf("cleanup during a backup: %+v %v", run, err)
	}

	database.backups.Add(-1)

	if run, err := database.Cleanup(ctx, 1); err != nil || run.Status == "" {
		t.Errorf("cleanup after the backup: %+v %v", run, err)
	}
}

// With the server stopped, the database is read without being written to.
func TestBackupStopped(t *testing.T) {
	dataDir := t.TempDir()
	database := openDisk(t, dataDir)

	saveEvent(t, database, "stopped", time.Now())

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	files := func() map[string]int64 {
		entries, _ := os.ReadDir(Location(dataDir))
		sizes := map[string]int64{}

		for _, entry := range entries {
			info, _ := entry.Info()
			sizes[entry.Name()] = info.Size()
		}

		return sizes
	}
	before := files()
	target := filepath.Join(t.TempDir(), "stopped")

	if _, err := BackupStopped(context.Background(), dataDir, target, nil); err != nil {
		t.Fatal(err)
	}

	if after := files(); !maps.Equal(before, after) {
		t.Errorf("the database files changed from %v to %v", before, after)
	}

	if issue := storedIssue(openDisk(t, target), "stopped"); issue == nil {
		t.Error("the backup misses the issue")
	}

	// Another format is reported as such, not as a database to move away.
	other := openDisk(t, t.TempDir())

	if err := other.db.Update(true, func(batch *kv.Batch) error { batch.Put(other.formatKey, []byte("0")); return nil }); err != nil {
		t.Fatal(err)
	}

	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := BackupStopped(context.Background(), other.dataDir, filepath.Join(t.TempDir(), "other"), nil)

	if !errors.Is(err, errFormat) || strings.Contains(err.Error(), "move") {
		t.Errorf("another format: %v", err)
	}
}

func TestBackupCopiesSecretKey(t *testing.T) {
	dataDir := t.TempDir()
	key, _, err := secret.KeyFile(dataDir)

	if err != nil {
		t.Fatal(err)
	}

	database := openDisk(t, dataDir)
	target := filepath.Join(t.TempDir(), "backup")
	result, err := database.Backup(context.Background(), target, nil)

	if err != nil {
		t.Fatal(err)
	}

	copied, created, err := secret.KeyFile(target)

	if err != nil || created || copied != key || !result.SecretKey {
		t.Errorf("copied key %q (created %v, %v), result %+v; want %q", copied, created, err, result, key)
	}

	info, err := os.Stat(filepath.Join(target, secret.KeyFileName))

	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v %v, want 0600", info.Mode(), err)
	}
}
