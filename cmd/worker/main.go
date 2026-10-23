package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitlab-doc-platform/internal/ai"
	"gitlab-doc-platform/internal/ai/queue"
	"gitlab-doc-platform/internal/analyzer"
	"gitlab-doc-platform/internal/config"
	"gitlab-doc-platform/internal/database"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/jobs"
	"gitlab-doc-platform/internal/observability"
	"gitlab-doc-platform/internal/publication/confluence"
	"gitlab-doc-platform/internal/publication/notion"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), time.Minute)
	db, err := database.Open(startupCtx, cfg.DatabaseURL)
	defer cancelStartup()
	if err != nil {
		logger.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	logger.Info("database ready", "store", "postgresql")
	store := jobs.NewPostgresStore(db)
	metrics := observability.New(store.QueuedCount)
	backend, err := newProvider(cfg, logger, metrics)
	if err != nil {
		logger.Error("LLM configuration invalid", "error", err)
		os.Exit(1)
	}
	logger.Info("AI provider configured", "provider", backend.Name(), "model", backend.Model(), "hosted", backend.Name() != "ollama")
	poolIdentity := strings.TrimRight(cfg.LLMEndpoint, "/")
	if cfg.LLMProvider != "ollama" {
		poolIdentity = cfg.LLMProvider + "\x00" + poolIdentity
	}
	poolHash := sha256.Sum256([]byte(poolIdentity))
	gate, err := queue.NewPostgres(startupCtx, db, hex.EncodeToString(poolHash[:]), cfg.LLMMaxConcurrent, cfg.LLMMaxQueued)
	if err != nil {
		logger.Error("inference queue initialization failed", "error", err)
		os.Exit(1)
	}
	provider := ai.QueuedProvider{Provider: backend, Gate: gate, Logger: logger}
	generator, err := documents.NewGeneratorWithOptions(provider, documents.Options{ContextLength: cfg.LLMContextLength, MaxTokens: cfg.LLMMaxTokens, Temperature: cfg.LLMTemperature, ConfigurationVersion: cfg.GenerationVersion()})
	if err != nil {
		logger.Error("generation configuration invalid", "error", err)
		os.Exit(1)
	}
	var publisher *notion.Runner
	if cfg.NotionEnabled {
		targets, token, err := notion.LoadFiles(cfg.NotionTargetsFile, cfg.NotionTokenFile)
		if err != nil {
			logger.Error("Notion configuration invalid", "error", err)
			os.Exit(1)
		}
		if err := notion.Configure(startupCtx, db, targets); err != nil {
			logger.Error("Notion target initialization failed")
			os.Exit(1)
		}
		publisher = &notion.Runner{DB: db, Client: notion.NewClient(token), Logger: logger, Interval: cfg.WorkerPollInterval}
	}
	var confluencePublisher *confluence.Runner
	if cfg.ConfluenceEnabled {
		settings, token, err := confluence.LoadFiles(cfg.ConfluenceConfigFile, cfg.ConfluenceTokenFile)
		if err != nil {
			logger.Error("Confluence configuration invalid", "error", err)
			os.Exit(1)
		}
		if err := confluence.Configure(startupCtx, db, settings); err != nil {
			logger.Error("Confluence target initialization failed")
			os.Exit(1)
		}
		confluencePublisher = &confluence.Runner{DB: db, Client: confluence.NewClient(settings, token), Logger: logger, Interval: cfg.WorkerPollInterval}
	}
	cancelStartup()
	worker := jobs.Worker{
		Store:          store,
		Analyzer:       analyzer.New(),
		Generator:      generator,
		Logger:         logger,
		Interval:       cfg.WorkerPollInterval,
		StorageDir:     cfg.StorageDir,
		ExtractedLimit: cfg.MaxExtractedBytes,
		JobTimeout:     cfg.WorkerJobTimeout,
		Metrics:        metrics,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var publicationDone sync.WaitGroup
	if publisher != nil {
		publicationDone.Add(1)
		go func() {
			defer publicationDone.Done()
			publisher.Run(ctx)
		}()
	}
	if confluencePublisher != nil {
		publicationDone.Add(1)
		go func() {
			defer publicationDone.Done()
			confluencePublisher.Run(ctx)
		}()
	}
	server := &http.Server{Addr: cfg.WorkerHTTPAddr, Handler: observability.HealthHandler(db.Ping, metrics), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker HTTP server failed", "error", err)
			stop()
		}
	}()
	logger.Info("worker started", "llm_request_timeout", cfg.LLMRequestTimeout, "job_timeout", cfg.WorkerJobTimeout, "inference_concurrency", cfg.LLMMaxConcurrent, "inference_queue_capacity", cfg.LLMMaxQueued, "context_length", cfg.LLMContextLength, "max_tokens", cfg.LLMMaxTokens)
	if err := worker.Run(ctx); err != nil {
		logger.Error("worker stopped with error", "error", err)
		os.Exit(1)
	}
	stop()
	publicationDone.Wait()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("worker HTTP shutdown failed", "error", err)
	}
}
