package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitlab-doc-platform/internal/api"
	"gitlab-doc-platform/internal/config"
	"gitlab-doc-platform/internal/database"
	"gitlab-doc-platform/internal/jobs"
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
	metrics := observability.New(store.QueuedCount)
	router := api.NewRouter(api.Dependencies{Config: cfg, Jobs: store, Logger: logger, ReadyCheck: db.Ping, Metrics: metrics})

	srv := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
	}

	go func() {
		logger.Info("api listening", "addr", cfg.APIAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("api failed", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
