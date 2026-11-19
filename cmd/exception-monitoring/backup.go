package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/server/control"
	"github.com/devforth/exception-monitoring/internal/storage/database"
)

// backup is `exception-monitoring backup [dir]`: a copy of the database, made by the running server or, when none runs, here.
func backup(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: exception-monitoring backup [absolute path of a new or empty directory; default DATA_DIR/backups/<time>]")
	}

	target := ""

	if len(args) == 1 {
		target = args[0]
	}

	settings, err := config.Load()

	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	defer stop()

	return runBackup(ctx, settings.DataDir, target, os.Stdout)
}

func runBackup(ctx context.Context, dataDir, target string, out io.Writer) error {
	if target == "" {
		base, err := filepath.Abs(filepath.Join(dataDir, "backups"))

		if err != nil {
			return err
		}

		target = filepath.Join(base, time.Now().UTC().Format("2006-01-02T150405Z"))
	}

	// The server resolves paths on its own, so a relative one would mean something else there.
	if !filepath.IsAbs(target) {
		return fmt.Errorf("the backup path %q is not absolute", target)
	}

	target = filepath.Clean(target)

	summary, err := control.Backup(ctx, dataDir, target, out)

	if errors.Is(err, control.ErrNoServer) {
		summary, err = backupOffline(ctx, dataDir, target, out, err)
	}

	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(out, summary)

	return nil
}

// backupOffline opens the database itself; noServer says why the socket did not answer.
func backupOffline(ctx context.Context, dataDir, target string, out io.Writer, noServer error) (string, error) {
	if _, err := os.Stat(database.Location(dataDir)); err != nil {
		return "", fmt.Errorf("no database in %s: %w", dataDir, err)
	}

	_, _ = fmt.Fprintln(out, "no server answers, the database is read here")

	result, err := database.BackupStopped(ctx, dataDir, target, backupProgress(func(text string) { _, _ = fmt.Fprintln(out, text) }))

	if errors.Is(err, database.ErrLocked) {
		return "", fmt.Errorf("the database is open in another process, but no server answers on %s (%w); stop that process and run the command again",
			control.SocketPath(dataDir), noServer)
	}

	if err != nil {
		return "", err
	}

	return describeBackup(target, result), nil
}

// listenControl opens the server's control socket; without it the server still runs, and backups need it stopped.
func listenControl(dataDir string, store *database.Store, logger *slog.Logger) *control.Server {
	server, err := control.Listen(dataDir, logger, backupHandler(store, logger))

	if err != nil {
		logger.Warn("control socket not available, the backup command needs the server stopped", "err", err)

		return nil
	}

	return server
}

func backupHandler(store *database.Store, logger *slog.Logger) control.BackupFunc {
	return func(ctx context.Context, target string, progress func(string)) (string, error) {
		logger.Info("backup started", "target", target)

		result, err := store.Backup(ctx, target, backupProgress(progress))

		if err != nil {
			logger.Warn("backup failed", "target", target, "err", err)

			return "", err
		}

		logger.Info("backup written", "target", target, "keys", result.Keys, "size", config.FormatSize(result.Size))

		return describeBackup(target, result), nil
	}
}

func backupProgress(report func(string)) func(keys, bytes int64) {
	return func(keys, bytes int64) {
		report(fmt.Sprintf("copied %d keys, %s", keys, config.FormatSize(bytes)))
	}
}

func describeBackup(target string, result database.BackupResult) string {
	summary := fmt.Sprintf("backup written to %s: %d keys, %s on disk", target, result.Keys, config.FormatSize(result.Size))

	if !result.SecretKey {
		summary += "; no secret.key in DATA_DIR: keep SECRET_KEY with the backup"
	}

	return summary
}
