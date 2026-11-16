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
	"github.com/devforth/exception-monitoring/internal/storage/projects"
	"github.com/devforth/exception-monitoring/internal/storage/retention"
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

	// `exception-monitoring reset-password [username]`: run on the server
	// (docker exec), it is how a forgotten password is recovered.
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

	logger := logging.New(os.Stderr, settings.LogFormat, settings.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	defer stop()

	database, err := store.Open(ctx, settings.DatabaseFile())

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

	if len(settings.AllowedIPs) == 0 {
		logger.Warn("ALLOWED_IPS is not set, the API and UI are reachable from any address")
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

	webUI, err := ui.New(ctx, logger, database, settings, registry, factory, authorization)

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

	// One catalog of requests, served as the REST API and as MCP tools, on
	// the same services as the UI.
	endpoints := catalog.New(database, registry, services.New(services.Dependencies{
		Logger: logger, Store: database, Registry: registry, Settings: settings, Factory: factory, OAuth: authorization,
	}))

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

// logRetention says at startup what is kept, so a surprising deletion can be
// traced to the settings.
func logRetention(logger *slog.Logger, settings config.Retention) {
	// Written as the settings take them; 0 means no limit.
	logger.Info("retention", logging.Block,
		"events", config.FormatDuration(settings.Events), "logs", config.FormatDuration(settings.Logs),
		"issues", config.FormatDuration(settings.Issues), "events_per_issue", settings.EventsPerIssue,
		"logs_per_message", settings.LogsPerMessage, "max_database_size", config.FormatSize(settings.MaxDatabaseBytes))
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
	available := false

	for _, channelType := range config.AllChannelTypes {
		state := "on"

		if ok, reason := settings.ChannelAvailable(channelType); ok {
			available = true
		} else {
			state = "off: " + reason
		}

		channels = append(channels, channelType, state)
	}

	logger.Info("notification channels", channels...)

	if settings.PublicURL == "" && available {
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
	database, err := store.Open(ctx, settings.DatabaseFile())

	if err != nil {
		return err
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
