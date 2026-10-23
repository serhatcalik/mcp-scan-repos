package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gitlab-doc-platform/internal/analyzer"
	"gitlab-doc-platform/internal/archive"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/evidence"
	"gitlab-doc-platform/internal/observability"
	"gitlab-doc-platform/internal/storage"
)

type Status string

const (
	Queued     Status = "queued"
	Analyzing  Status = "analyzing"
	Generating Status = "generating"
	Validating Status = "validating"
	Completed  Status = "completed"
	Failed     Status = "failed"
	Cancelled  Status = "cancelled"
)

type Job struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"project_id"`
	ProjectName    string    `json:"project_name"`
	GroupID        string    `json:"group_id"`
	GroupName      string    `json:"group_name,omitempty"`
	CommitSHA      string    `json:"commit_sha"`
	Branch         string    `json:"branch"`
	IdempotencyKey string    `json:"idempotency_key"`
	ArchivePath    string    `json:"archive_path"`
	Status         Status    `json:"status"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Store interface {
	CreateOrGet(ctx context.Context, job Job) (Job, bool, error)
	Get(ctx context.Context, id string) (Job, error)
	LeaseNext(ctx context.Context, groups []string, leaseFor time.Duration) (Job, bool, error)
	UpdateStatus(ctx context.Context, id string, status Status, message string) error
	SaveAnalysis(ctx context.Context, id string, snapshot evidence.Snapshot) error
	GetAnalysis(ctx context.Context, id string) (evidence.Snapshot, error)
	SaveDocuments(ctx context.Context, id string, artifacts documents.ArtifactSet) error
}

var (
	ErrNotFound          = errors.New("job not found")
	ErrGroupDisabled     = errors.New("group is disabled")
	ErrProjectNotAllowed = errors.New("project is not allowed by group policy")
	ErrSnapshotIdentity  = errors.New("analysis identity does not match job")
)

type MemoryStore struct {
	mu        sync.Mutex
	jobs      map[string]Job
	keys      map[string]string
	snapshots map[string][]byte
	documents map[string]map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: map[string]Job{}, keys: map[string]string{}, snapshots: map[string][]byte{}, documents: map[string]map[string]string{}}
}

func (s *MemoryStore) CreateOrGet(ctx context.Context, job Job) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.keys[job.IdempotencyKey]; id != "" {
		existing := s.jobs[id]
		if existing.Status == Failed {
			existing.Status, existing.Error, existing.UpdatedAt = Queued, "", time.Now().UTC()
			s.jobs[id] = existing
		}
		return existing, false, nil
	}
	now := time.Now().UTC()
	job.CreatedAt, job.UpdatedAt, job.Status = now, now, Queued
	s.jobs[job.ID] = job
	s.keys[job.IdempotencyKey] = job.ID
	return job, true, nil
}

func (s *MemoryStore) Get(ctx context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return job, nil
}

func (s *MemoryStore) LeaseNext(ctx context.Context, groups []string, leaseFor time.Duration) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, job := range s.jobs {
		if job.Status == Queued {
			job.Status = Analyzing
			job.UpdatedAt = time.Now().UTC()
			s.jobs[id] = job
			return job, true, nil
		}
	}
	return Job{}, false, nil
}

func (s *MemoryStore) UpdateStatus(ctx context.Context, id string, status Status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return ErrNotFound
	}
	job.Status, job.Error, job.UpdatedAt = status, message, time.Now().UTC()
	s.jobs[id] = job
	return nil
}

type Worker struct {
	Store          Store
	Analyzer       analyzer.Analyzer
	Generator      documents.Generator
	Logger         *slog.Logger
	Interval       time.Duration
	StorageDir     string
	ExtractedLimit int64
	JobTimeout     time.Duration
	Metrics        *observability.Metrics
}

func (s *MemoryStore) SaveAnalysis(ctx context.Context, id string, snapshot evidence.Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return ErrNotFound
	}
	if job.ProjectID != snapshot.ProjectID || job.CommitSHA != snapshot.CommitSHA {
		return ErrSnapshotIdentity
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	s.snapshots[id] = data
	return nil
}

func (s *MemoryStore) GetAnalysis(ctx context.Context, id string) (evidence.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return evidence.Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.snapshots[id]
	if !ok {
		return evidence.Snapshot{}, ErrNotFound
	}
	var snapshot evidence.Snapshot
	err := json.Unmarshal(data, &snapshot)
	return snapshot, err
}

func (w Worker) Run(ctx context.Context) error {
	if w.Interval <= 0 {
		w.Interval = 5 * time.Second
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		if err := w.processOne(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.Logger.Error("job processing failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w Worker) processOne(ctx context.Context) error {
	timeout := w.JobTimeout
	if timeout <= 0 {
		timeout = 75 * time.Minute
	}
	job, ok, err := w.Store.LeaseNext(ctx, nil, timeout+time.Minute)
	if err != nil || !ok {
		return err
	}
	logger := w.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("job_id", job.ID, "project_id", job.ProjectID, "group_id", job.GroupID, "commit_sha", job.CommitSHA)
	started := time.Now()
	if w.Metrics != nil {
		w.Metrics.Active.Inc()
		defer w.Metrics.Active.Dec()
		defer func() { w.Metrics.JobDuration.Observe(time.Since(started).Seconds()) }()
	}
	logger.Info("job leased", "status", Analyzing, "timeout", timeout)
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := w.processJob(jobCtx, job, logger); err != nil {
		// Persist failure even when generation's deadline or shutdown canceled jobCtx.
		failureCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if updateErr := w.Store.UpdateStatus(failureCtx, job.ID, Failed, err.Error()); updateErr != nil {
			logger.Error("failed to persist job failure", "error", updateErr)
		}
		logger.Error("job failed", "error", err, "duration", time.Since(started))
		if w.Metrics != nil {
			w.Metrics.Failed.Inc()
		}
		return err
	}
	logger.Info("job completed", "duration", time.Since(started))
	if w.Metrics != nil {
		w.Metrics.Jobs.WithLabelValues("completed").Inc()
	}
	return nil
}

func (w Worker) processJob(ctx context.Context, job Job, logger *slog.Logger) error {
	if job.ArchivePath == "" {
		return errors.New("archive path is not configured")
	}
	workdir, err := os.MkdirTemp("", "gitlab-doc-job-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workdir)
	archiveFile, err := os.Open(job.ArchivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	logger.Info("extracting archive")
	err = archive.ExtractTarGz(archiveFile, workdir, archive.Limits{MaxExtractedBytes: w.ExtractedLimit, MaxFiles: 20000})
	_ = archiveFile.Close()
	if err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}
	logger.Info("analyzing repository")
	analysisStarted := time.Now()
	snap, err := w.Analyzer.Analyze(ctx, workdir, analyzer.Metadata{ProjectID: job.ProjectID, CommitSHA: job.CommitSHA})
	if w.Metrics != nil {
		w.Metrics.AnalysisDuration.Observe(time.Since(analysisStarted).Seconds())
	}
	if err != nil {
		return fmt.Errorf("analyze repository: %w", err)
	}
	snap = documents.SanitizeSnapshot(snap)
	if err := w.Store.SaveAnalysis(ctx, job.ID, snap); err != nil {
		return fmt.Errorf("persist analysis: %w", err)
	}
	logger.Info("analysis persisted", "evidence_items", len(snap.Evidence), "inventory_files", len(snap.Inventory), "warnings", len(snap.Warnings))
	if err := w.Store.UpdateStatus(ctx, job.ID, Generating, ""); err != nil {
		return err
	}
	logger.Info("generating documents", "evidence_items", len(snap.Evidence))
	artifacts, err := w.Generator.Generate(ctx, snap, documents.Metadata{JobID: job.ID, ProjectID: job.ProjectID, ProjectName: job.ProjectName, CommitSHA: job.CommitSHA, Branch: job.Branch})
	if err != nil {
		return fmt.Errorf("generate documents: %w", err)
	}
	if err := w.Store.UpdateStatus(ctx, job.ID, Validating, ""); err != nil {
		return err
	}
	logger.Info("writing artifacts")
	if err := w.Store.SaveDocuments(ctx, job.ID, artifacts); err != nil {
		return fmt.Errorf("persist documents: %w", err)
	}
	artifactPath := filepath.Join(w.StorageDir, "artifacts", job.ID+".tar.gz")
	if err := storage.WriteArtifactBundle(artifactPath, artifacts); err != nil {
		return fmt.Errorf("write artifacts: %w", err)
	}
	return w.Store.UpdateStatus(ctx, job.ID, Completed, "")
}
