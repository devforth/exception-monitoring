package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/devforth/exception-monitoring/internal/platform/secret"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// ErrLocked: another process (a running server) holds the database.
var ErrLocked = kv.ErrLocked

// Location is the database directory in dataDir.
func Location(dataDir string) string { return filepath.Join(dataDir, engineName) }

// BackupResult is what a backup holds.
type BackupResult struct {
	Keys, Bytes int64 // Bytes: keys and values before compression
	Size        int64 // on disk
	SecretKey   bool  // DATA_DIR/secret.key was copied
}

// backupProgressBytes is how often a backup reports progress.
var backupProgressBytes int64 = 64 << 20

// snapshotter is a kv.DB or a kv.Engine.
type snapshotter interface {
	Snapshot() (kv.Snapshot, error)
}

// Backup writes a copy of the database at one moment into target (see writeBackup) while the store keeps working.
func (database *Store) Backup(ctx context.Context, target string, progress func(keys, bytes int64)) (BackupResult, error) {
	if database.dataDir == "" {
		return BackupResult{}, errors.New("a store in memory has no backup")
	}

	database.backups.Add(1)

	defer database.backups.Add(-1)

	return writeBackup(ctx, database.db, database.dataDir, target, progress)
}

// BackupStopped writes a backup of the database in dataDir that no server holds, opened only to read.
func BackupStopped(ctx context.Context, dataDir, target string, progress func(keys, bytes int64)) (result BackupResult, err error) {
	engine, err := openEngineReadOnly(Location(dataDir))

	if err != nil {
		return result, fmt.Errorf("open database: %w", err)
	}

	defer func() { err = errors.Join(err, engine.Close()) }()

	if err := readFormat(engine); err != nil {
		return result, err
	}

	return writeBackup(ctx, engine, dataDir, target, progress)
}

// writeBackup copies a snapshot of source and the secret.key of dataDir into target, usable as a DATA_DIR.
// target must be new or empty; target/<engine> appears only when the backup is whole.
func writeBackup(ctx context.Context, source snapshotter, dataDir, target string, progress func(keys, bytes int64)) (result BackupResult, err error) {
	target, err = checkBackupTarget(target, Location(dataDir))

	if err != nil {
		return result, err
	}

	created, err := makeTarget(target)

	if err != nil {
		return result, err
	}

	// Built inside target, so the renames stay on its filesystem (a mount point too).
	partial, err := os.MkdirTemp(target, ".partial-")

	defer func() {
		if err != nil {
			_ = os.RemoveAll(partial)

			if created {
				_ = os.Remove(target)
			}
		}
	}()

	if err != nil {
		return result, err
	}

	result, err = copySnapshot(ctx, source, filepath.Join(partial, engineName), progress)

	if err != nil {
		return result, err
	}

	result.SecretKey, err = copyFile(filepath.Join(dataDir, secret.KeyFileName), filepath.Join(partial, secret.KeyFileName))

	if err != nil {
		return result, fmt.Errorf("copy %s: %w", secret.KeyFileName, err)
	}

	if result.Size, err = syncTree(partial); err != nil {
		return result, err
	}

	if err = publish(partial, target); err != nil {
		return result, err
	}

	if created {
		err = syncPath(filepath.Dir(target))
	}

	return result, err
}

// copySnapshot copies a snapshot of every key into a new engine in dir.
func copySnapshot(ctx context.Context, source snapshotter, dir string, progress func(keys, bytes int64)) (BackupResult, error) {
	var result BackupResult

	snapshot, err := source.Snapshot()

	if err != nil {
		return result, err
	}

	defer snapshot.Release()

	engine, err := openEngine(dir)

	if err != nil {
		return result, err
	}

	reported := int64(0)
	result.Keys, result.Bytes, err = kv.Copy(ctx, snapshot, engine, func(keys, bytes int64) {
		if progress != nil && bytes-reported >= backupProgressBytes {
			reported = bytes
			progress(keys, bytes)
		}
	})

	return result, errors.Join(err, engine.Close())
}

// checkBackupTarget refuses a relative path, a non-empty one and one inside the database; it returns the path with links resolved.
func checkBackupTarget(target, location string) (string, error) {
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("the backup path %q is not absolute", target)
	}

	resolved, err := resolveLinks(filepath.Clean(target))

	if err != nil {
		return "", err
	}

	if location, err = filepath.EvalSymlinks(location); err != nil {
		return "", err
	}

	if inside, err := filepath.Rel(location, resolved); err == nil && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is inside the database", target)
	}

	entries, err := os.ReadDir(resolved)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return resolved, nil
	case err != nil:
		return "", fmt.Errorf("%s: %w", target, err)
	case len(entries) > 0:
		return "", fmt.Errorf("%s is not empty", target)
	}

	return resolved, nil
}

// resolveLinks follows the symbolic links of the longest part of path that exists.
func resolveLinks(path string) (string, error) {
	missing := ""

	for {
		resolved, err := filepath.EvalSymlinks(path)

		if err == nil {
			return filepath.Join(resolved, missing), nil
		}

		parent := filepath.Dir(path)

		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return "", err
		}

		missing = filepath.Join(filepath.Base(path), missing)
		path = parent
	}
}

// makeTarget creates target unless it exists (checked empty); true when created here.
func makeTarget(target string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, err
	}

	err := os.Mkdir(target, 0o700)

	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}

	return err == nil, err
}

// publish moves the backup from partial into target, the database last, and removes partial.
func publish(partial, target string) error {
	entries, err := os.ReadDir(target)

	if err != nil {
		return err
	}

	if len(entries) != 1 {
		return fmt.Errorf("%s changed during the backup", target)
	}

	movedKey := filepath.Join(target, secret.KeyFileName)

	if err := os.Rename(filepath.Join(partial, secret.KeyFileName), movedKey); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		movedKey = ""
	}

	if err := os.Rename(filepath.Join(partial, engineName), filepath.Join(target, engineName)); err != nil {
		if movedKey != "" {
			_ = os.Remove(movedKey)
		}

		return err
	}

	if err := os.Remove(partial); err != nil {
		return err
	}

	return syncPath(target)
}

// copyFile copies source to a new file (owner only); false when there is no source.
func copyFile(source, destination string) (bool, error) {
	input, err := os.Open(source)

	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	defer func() { _ = input.Close() }() // read only

	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)

	if err != nil {
		return false, err
	}

	_, err = io.Copy(output, input)

	return true, errors.Join(err, output.Close())
}

// syncTree flushes every file and directory under root to the disk and returns the size of the files.
func syncTree(root string) (int64, error) {
	var size int64

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.Type().IsRegular() {
			info, err := entry.Info()

			if err != nil {
				return err
			}

			size += info.Size()
		}

		return syncPath(path)
	})

	return size, err
}

func syncPath(path string) error {
	file, err := os.Open(path)

	if err != nil {
		return err
	}

	return errors.Join(file.Sync(), file.Close())
}
