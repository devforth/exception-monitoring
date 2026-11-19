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
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/devforth/exception-monitoring/internal/api/catalog"
	"github.com/devforth/exception-monitoring/internal/api/mcp"
	"github.com/devforth/exception-monitoring/internal/api/oauth"
	"github.com/devforth/exception-monitoring/internal/api/rest"
	"github.com/devforth/exception-monitoring/internal/ingest"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/logging"
	"github.com/devforth/exception-monitoring/internal/platform/secret"
	"github.com/devforth/exception-monitoring/internal/server"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/database"
	"github.com/devforth/exception-monitoring/internal/storage/projects"
	"github.com/devforth/exception-monitoring/internal/storage/store"
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

	// `exception-monitoring reset-password [username]`: run with the server
	// stopped (it holds the database), it is how a forgotten password is recovered.
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		if err := resetPassword(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}

		return
	}

	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)

		return
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

	config.LimitMemory()

	logger := logging.New(os.Stderr, settings.LogFormat, settings.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	defer stop()

	database, err := database.Open(settings.DataDir)

	if err != nil {
		return err
	}

	defer func() {
		// Closing flushes the store; a failure here is worth knowing about.
		if err := database.Close(); err != nil {
			logger.Error("close database", "err", err)
		}
	}()

	if settings.APIToken == "" {
		logger.Warn("API_TOKEN is not set, the REST API is closed")
	}

	if len(settings.AllowedIPs) == 0 {
		logger.Warn("ALLOWED_IPS is not set, the API and UI are reachable from any address")
	}

	// Chat tokens are stored encrypted: without SECRET_KEY the key is the one
	// generated into the data directory.
	if settings.Notifications.SecretKey == "" {
		key, created, err := secret.KeyFile(settings.DataDir)

		if err != nil {
			return fmt.Errorf("secret key: %w", err)
		}

		if created {
			logger.Info("secret key generated: set SECRET_KEY to keep it outside the data directory", "file", filepath.Join(settings.DataDir, secret.KeyFileName))
		}

		settings.Notifications.SecretKey = key
	}

	factory, err := notificationFactory(logger, settings.AppName, settings.Notifications)

	if err != nil {
		return err
	}

	// Projects live in the database; the ones in PROJECTS are written there
	// with the same values at every start.
	registry, err := projects.Open(ctx, logger, database, settings.Projects)

	if err != nil {
		return err
	}

	// Agents sign in to the MCP server with OAuth; the UI holds the consent
	// page and the list of connected agents.
	authorization := &oauth.Server{
		Name: settings.AppName, Store: database, PublicURL: settings.Notifications.PublicURL, TrustedProxies: settings.TrustedProxies, ResourcePath: mcp.Path,
	}

	// The rules of the app, one set for the UI and for agents' tools and the REST API.
	all := services.New(services.Dependencies{
		Logger: logger, Store: database, Registry: registry, Settings: settings, Factory: factory, OAuth: authorization,
	})

	webUI, err := ui.New(ctx, logger, database, settings, registry, factory, all)

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
		// After the first size check and before the first inbox compaction (config.CleanupInterval).
		CleanupDelay: config.OutboxCleanupDelay,
	})

	defer func() {
		// Send what is still queued; give up after a while so shutdown cannot hang.
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		defer cancel()

		if err := notifier.Close(closeCtx); err != nil {
			logger.Warn("notifications not sent before shutdown", "err", err)
		}
	}()

	// The cleanup keeps the database under MAX_DATABASE_SIZE, in the
	// background until shutdown; it stops before the database closes
	// (defers run in reverse order).
	cleanupCtx, stopCleanup := context.WithCancel(ctx)
	cleanupDone := make(chan struct{})

	go func() {
		defer close(cleanupDone)

		database.RunCleanup(cleanupCtx, settings.Retention.MaxDatabaseBytes, config.CleanupInterval, logger)
	}()

	defer func() {
		stopCleanup()
		<-cleanupDone
	}()

	logRetention(logger, settings.Retention)

	// One catalog of requests, served as the REST API and as MCP tools, on
	// the same services as the UI.
	endpoints := catalog.New(database, registry, all)

	sink := ingest.StoreSink{Store: database, Logger: logger, Notifier: notifier, PublicURL: settings.Notifications.PublicURL,
		Recording: settings.Recording}

	// Envelopes go through the inbox: the handler answers once one is on
	// disk, the pipeline stores it. It stops after the HTTP server (no new
	// envelopes) and before the notifier and the database (defers run in
	// reverse order), storing what it holds.
	ingestHandler := ingest.New(logger, settings, registry, sink)
	pipeline := ingest.NewPipeline(ingestHandler, database, sink)
	pipelineCtx, stopPipeline := context.WithCancel(context.Background())
	pipelineDone := make(chan struct{})

	go func() {
		defer close(pipelineDone)

		pipeline.Run(pipelineCtx)
	}()

	defer func() {
		stopPipeline()
		<-pipelineDone
	}()

	// Bind before logging "started" so a busy port fails here, not after a false start message.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", settings.Port))

	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler: server.New(server.Options{
			Logger:         logger,
			Database:       database,
			Ingest:         ingestHandler,
			API:            rest.New(logger, endpoints, settings.Notifications.PublicURL, settings.TrustedProxies),
			MCP:            mcp.New(logger, database, endpoints, authorization, settings.AppName, version),
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

	logger.Info("server started", logging.Block, "url", serverURL(settings, listener.Addr()), "version", version, "data_dir", settings.DataDir, "projects", len(registry.List()))

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

// serverURL is where to open the server: PUBLIC_URL, or this machine at
// the port it listens on.
func serverURL(settings *config.Config, address net.Addr) string {
	if settings.Notifications.PublicURL != "" {
		return settings.Notifications.PublicURL
	}

	if tcp, ok := address.(*net.TCPAddr); ok {
		return "http://localhost:" + strconv.Itoa(tcp.Port)
	}

	return address.String()
}

// logRetention says at startup how the database is kept small, so a
// surprising deletion can be traced to the setting.
func logRetention(logger *slog.Logger, settings config.Retention) {
	if settings.MaxDatabaseBytes == 0 {
		logger.Info("retention", "max_database_size", "0 (nothing is deleted)")

		return
	}

	logger.Info("retention", logging.Block, "max_database_size", config.FormatSize(settings.MaxDatabaseBytes),
		"cleanup", fmt.Sprintf("from %d%% to %d%%", config.CleanupStartPercent, config.CleanupStopPercent))
}

// notificationFactory builds channels for the connections set up in the UI.
// It logs which connection types can be created with the current settings.
func notificationFactory(logger *slog.Logger, appName string, settings config.Notifications) (notify.Factory, error) {
	factory := notify.Factory{AppName: appName, SMTP: settings.SMTP}

	if settings.SecretKey != "" {
		box, err := secret.New(settings.SecretKey)

		if err != nil {
			return factory, fmt.Errorf("SECRET_KEY: %w", err)
		}

		factory.Box = box
	}

	// One field per type: "on", or what is missing.
	channels := []any{logging.Block}

	for _, channelType := range config.AllChannelTypes {
		state := "on"

		if ok, reason := settings.ChannelAvailable(channelType); !ok {
			state = "off: " + reason
		}

		channels = append(channels, channelType, state)
	}

	logger.Info("notification channels", channels...)

	// Telegram and Slack are always available (the secret key is generated when not set).
	if settings.PublicURL == "" {
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

// resetPassword gives a user (the superadmin when no username is given) a
// temporary password and prints it: they choose their own at the next
// login. Their sessions end; connected agents keep working.
func resetPassword(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: exception-monitoring reset-password [username]")
	}

	settings, err := config.Load()

	if err != nil {
		return err
	}

	ctx := context.Background()
	logger := logging.New(os.Stderr, settings.LogFormat, settings.LogLevel)
	// The running server holds the database: the command runs while it is stopped.
	database, err := database.Open(settings.DataDir)

	if err != nil {
		return fmt.Errorf("stop the server first, the database is open there (%w)", err)
	}

	defer func() { _ = database.Close() }()

	username := ""

	if len(args) == 1 {
		username = args[0]
	} else {
		superadmin, err := database.Superadmin(ctx)

		if err != nil {
			return fmt.Errorf("no superadmin yet: set ADMIN_PASSWORD and start the server: %w", err)
		}

		username = superadmin.Username
	}

	users := services.NewUsers(logger, database)
	password, err := users.SetPasswordByOperator(ctx, username)

	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user %q", username)
	}

	if err != nil {
		return err
	}

	fmt.Printf("Temporary password of %s: %s\nLog in with it and choose a new password.\n", username, password)

	return nil
}
