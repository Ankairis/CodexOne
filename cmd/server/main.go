package main

import (
	"context"
	"errors"
	"fmt"
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

	retentionBefore := time.Now().AddDate(0, 0, -cfg.RequestRetentionDays).UnixMilli()
	if count, cleanupErr := database.DeleteOldRequestLogs(ctx, retentionBefore); cleanupErr != nil {
		logger.Warn("request log cleanup failed", "error", cleanupErr)
	} else if count > 0 {
		logger.Info("old request logs removed", "count", count)
	}

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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
