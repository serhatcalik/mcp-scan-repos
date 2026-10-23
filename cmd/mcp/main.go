package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitlab-doc-platform/internal/config"
	"gitlab-doc-platform/internal/database"
	"gitlab-doc-platform/internal/jobs"
	mcpserver "gitlab-doc-platform/internal/mcp"
	"gitlab-doc-platform/internal/observability"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), time.Minute)
	db, err := database.Open(startupCtx, cfg.DatabaseURL)
	cancelStartup()
	if err != nil {
		logger.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	logger.Info("database ready", "store", "postgresql")
	store := jobs.NewPostgresStore(db)
	file, err := os.Open(cfg.MCPAuthFile)
	if err != nil {
		logger.Error("MCP auth file unavailable")
		os.Exit(1)
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	_ = file.Close()
	if err != nil || len(data) > 1<<20 {
		logger.Error("MCP auth file unreadable or oversized")
		os.Exit(1)
	}
	credentials, err := mcpserver.ParseCredentials(data)
	if err != nil {
		logger.Error("MCP authentication configuration invalid", "error", err)
		os.Exit(1)
	}
	metrics := observability.New(store.QueuedCount)
	options := mcpserver.Options{Credentials: credentials, GenerationVersion: cfg.GenerationVersion(), ReadyCheck: db.Ping, Logger: logger, Metrics: metrics}
	handler := mcpserver.NewHandler(store, options)

	srv := &http.Server{
		Addr:              cfg.MCPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
	}
	management := &http.Server{
		Addr:              cfg.MCPManagementAddr,
		Handler:           mcpserver.NewManagementHandler(options),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
	}
	go func() {
		logger.Info("mcp management listening", "addr", cfg.MCPManagementAddr)
		if err := management.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("mcp management failed", "error", err)
			os.Exit(1)
		}
	}()

	go func() {
		logger.Info("mcp listening", "addr", cfg.MCPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("mcp failed", "error", err)
			os.Exit(1)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = management.Shutdown(shutdownCtx)
}
