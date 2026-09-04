package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Ankairis/CodexOne/internal/codex"
	"github.com/Ankairis/CodexOne/internal/config"
	"github.com/Ankairis/CodexOne/internal/cryptox"
	appLog "github.com/Ankairis/CodexOne/internal/logging"
	"github.com/Ankairis/CodexOne/internal/proxy"
	"github.com/Ankairis/CodexOne/internal/session"
	"github.com/Ankairis/CodexOne/internal/store"
	"github.com/Ankairis/CodexOne/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "codexone: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger, ring, logCloser, err := appLog.New(cfg.LogPath)
	if err != nil {
		return fmt.Errorf("initialize logging: %w", err)
	}
	defer logCloser.Close()
	ctx := context.Background()
	database, err := store.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	sessions, err := session.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer sessions.Close()
	cipher, err := cryptox.New(cfg)
	if err != nil {
		return err
	}
	initialPassword, generated, err := web.EnsureAdminPassword(ctx, database, cfg.AdminPassword)
	if err != nil {
		return fmt.Errorf("initialize admin password: %w", err)
	}
	if generated {
		fmt.Fprintf(os.Stderr, "\nCodexOne initial admin password: %s\nChange it after first sign-in. This value will not be shown again.\n\n", initialPassword)
	}

	codexManager := codex.NewManager(cfg, database, cipher, sessions, logger)
	proxyService := proxy.New(cfg, codexManager, database, logger)
	handler := web.New(cfg, database, sessions, codexManager, proxyService, logger, ring)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	cleanupRequestLogs(ctx, database, cfg.RequestRetentionDays, time.Now(), logger)
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	cleanupTicker := time.NewTicker(24 * time.Hour)
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		runRequestLogCleanup(cleanupCtx, database, cfg.RequestRetentionDays, cleanupTicker.C, logger)
	}()
	defer func() {
		cleanupTicker.Stop()
		stopCleanup()
		<-cleanupDone
	}()

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("CodexOne started", "address", cfg.Addr, "base_url", cfg.PublicURL+"/v1", "storage", cfg.StorageDriver)
		serverErrors <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case signalValue := <-stop:
		logger.Info("shutdown requested", "signal", signalValue.String())
	case serveErr := <-serverErrors:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", serveErr)
		}
		return nil
	}
	proxyService.BeginShutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		logger.Warn("graceful HTTP shutdown timed out; closing active connections", "error", shutdownErr)
		if closeErr := server.Close(); closeErr != nil {
			logger.Warn("force-close HTTP server failed", "error", closeErr)
		}
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	drainErr := proxyService.WaitForIdle(drainCtx)
	cancelDrain()
	if drainErr != nil {
		return fmt.Errorf("wait for proxy request logs: %w", drainErr)
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return fmt.Errorf("shutdown HTTP server: %w", shutdownErr)
	}
	return nil
}

type requestLogCleaner interface {
	DeleteOldRequestLogs(context.Context, int64) (int64, error)
}

func runRequestLogCleanup(ctx context.Context, database requestLogCleaner, retentionDays int, ticks <-chan time.Time, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			cleanupRequestLogs(ctx, database, retentionDays, now, logger)
		}
	}
}

func cleanupRequestLogs(ctx context.Context, database requestLogCleaner, retentionDays int, now time.Time, logger *slog.Logger) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	retentionBefore := now.AddDate(0, 0, -retentionDays).UnixMilli()
	if count, err := database.DeleteOldRequestLogs(cleanupCtx, retentionBefore); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Warn("request log cleanup failed", "error", err)
		}
	} else if count > 0 {
		logger.Info("old request logs removed", "count", count)
	}
}
