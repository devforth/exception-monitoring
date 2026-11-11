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

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/ingest"
	"github.com/devforth/exception-monitoring/internal/server"
	"github.com/devforth/exception-monitoring/internal/store"
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
	defer database.Close()

	if len(settings.Projects) == 0 {
		logger.Warn("no projects configured, events will be rejected; set PROJECTS=id:name:public_key,...")
	}

	// Bind before logging "started" so a busy port fails here, not after a false start message.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", settings.Port))
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		// LogSink only logs received data; storing it comes with the data model.
		Handler:           server.New(logger, database, ingest.New(logger, settings, ingest.LogSink{Logger: logger})).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("server started", "version", version, "addr", listener.Addr().String(), "data_dir", settings.DataDir, "projects", len(settings.Projects))

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
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", response.Status)
		return 1
	}
	return 0
}
