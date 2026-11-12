package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/devforth/exception-monitoring/internal/api"
	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/ingest"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/projects"
	"github.com/devforth/exception-monitoring/internal/retention"
	"github.com/devforth/exception-monitoring/internal/secret"
	"github.com/devforth/exception-monitoring/internal/server"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui"
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	// `exception-monitoring healthcheck` is used by Docker HEALTHCHECK:
	// the image is FROM scratch, so there is no curl/wget inside.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	settings, err := config.Load()

	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: settings.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	defer stop()

	database, err := store.Open(ctx, settings.DataDir)

	if err != nil {
		return err
	}

	defer func() {
		// Closing flushes the WAL into the database file; a failure here is worth knowing about.
		if err := database.Close(); err != nil {
			logger.Error("close database", "err", err)
		}
	}()

	if settings.APIToken == "" {
		logger.Warn("API_TOKEN is not set, the REST API is closed")
	}

	if settings.AdminPassword == "" {
		logger.Warn("ADMIN_PASSWORD is not set, the web UI is closed")
	}

	if len(settings.AllowedIPs) == 0 {
		logger.Warn("ALLOWED_IPS is not set, the API and UI are reachable from any address")
	}

	factory, err := notificationFactory(logger, settings.Notifications)

	if err != nil {
		return err
	}

	// Projects live in the database; the ones in PROJECTS are written there
	// with the same values at every start.
	registry, err := projects.Open(ctx, logger, database, settings.Projects)

	if err != nil {
		return err
	}

	webUI, err := ui.New(ctx, logger, database, settings, registry, factory)

	if err != nil {
		return err
	}

	if len(registry.List()) == 0 {
		logger.Warn("no projects yet, events will be rejected; add one on the Projects page or set PROJECTS=id:name:public_key,...")
	}

	notifier := notify.New(notify.Options{
		Store:      database,
		Channels:   factory,
		Logger:     logger,
		RetryFor:   settings.Notifications.RetryFor,
		KeepSent:   settings.Notifications.KeepSent,
		KeepFailed: settings.Notifications.KeepFailed,
	})

	defer func() {
		// Send what is still queued; give up after a while so shutdown cannot hang.
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		defer cancel()

		if err := notifier.Close(closeCtx); err != nil {
			logger.Warn("notifications not sent before shutdown", "err", err)
		}
	}()

	// Retention runs in the background until shutdown; it stops before the
	// database closes (defers run in reverse order).
	retentionCtx, stopRetention := context.WithCancel(ctx)
	retentionDone := make(chan struct{})

	go func() {
		defer close(retentionDone)

		cleaner := retention.Cleaner{Store: database, Settings: settings.Retention, Projects: registry, Logger: logger}
		cleaner.Run(retentionCtx)
	}()

	defer func() {
		stopRetention()
		<-retentionDone
	}()

	logRetention(logger, settings.Retention)

	sink := ingest.StoreSink{Store: database, Logger: logger, Notifier: notifier, PublicURL: settings.Notifications.PublicURL,
		Retention: settings.Retention, Recording: settings.Recording}

	// Bind before logging "started" so a busy port fails here, not after a false start message.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", settings.Port))

	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler: server.New(server.Options{
			Logger:         logger,
			Database:       database,
			Ingest:         ingest.New(logger, settings, registry, sink),
			API:            api.New(logger, database, registry),
			UI:             webUI,
			APIToken:       settings.APIToken,
			AllowedIPs:     settings.AllowedIPs,
			TrustedProxies: settings.TrustedProxies,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("server started", "version", version, "addr", listener.Addr().String(), "data_dir", settings.DataDir, "projects", len(registry.List()))

	serveResult := make(chan error, 1)

	go func() {
		serveResult <- httpServer.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}

	return nil
}

// logRetention says at startup what is kept, so a surprising deletion can be
// traced to the settings.
func logRetention(logger *slog.Logger, settings config.Retention) {
	// Written as the settings take them; 0 means no limit.
	logger.Info("retention",
		"events", config.FormatDuration(settings.Events), "logs", config.FormatDuration(settings.Logs),
		"issues", config.FormatDuration(settings.Issues), "events_per_issue", settings.EventsPerIssue,
		"logs_per_message", settings.LogsPerMessage, "max_database_size", config.FormatSize(settings.MaxDatabaseBytes))
}

// notificationFactory builds channels for the connections set up in the UI.
// It logs which connection types can be created with the current settings.
func notificationFactory(logger *slog.Logger, settings config.Notifications) (notify.Factory, error) {
	factory := notify.Factory{SMTP: settings.SMTP}

	if settings.SecretKey != "" {
		box, err := secret.New(settings.SecretKey)

		if err != nil {
			return factory, fmt.Errorf("SECRET_KEY: %w", err)
		}

		factory.Box = box
	}

	var available, unavailable []string

	for _, channelType := range config.AllChannelTypes {
		if ok, reason := settings.ChannelAvailable(channelType); ok {
			available = append(available, channelType)
		} else {
			unavailable = append(unavailable, channelType+" ("+reason+")")
		}
	}

	logger.Info("notification connection types", "available", strings.Join(available, ", "), "unavailable", strings.Join(unavailable, "; "))

	if settings.PublicURL == "" && len(available) > 0 {
		logger.Warn("PUBLIC_URL is not set, notifications will not link to the issue")
	}

	return factory, nil
}

func healthcheck() int {
	port := os.Getenv("PORT")

	if port == "" {
		port = strconv.Itoa(config.DefaultPort)
	}

	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + port + "/health")

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	defer func() { _ = response.Body.Close() }() // read-only, nothing to lose on close

	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", response.Status)
		return 1
	}

	return 0
}
