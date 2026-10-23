package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gitlab-doc-platform/internal/evidence"
	"gitlab-doc-platform/internal/jobs"
	"gitlab-doc-platform/internal/observability"
)

type catalog interface {
	Get(context.Context, string) (jobs.Job, error)
	CreateOrGet(context.Context, jobs.Job) (jobs.Job, bool, error)
	GetAnalysis(context.Context, string) (evidence.Snapshot, error)
	GetDocument(context.Context, string, string) (string, error)
	ListJobs(context.Context, []string, string, int, int) ([]jobs.Job, error)
	ListProjects(context.Context, []string, int, int) ([]jobs.Project, error)
}
type Options struct {
	Credentials       []Credential
	GenerationVersion string
	ReadyCheck        func(context.Context) error
	Logger            *slog.Logger
	Metrics           *observability.Metrics
	RequestsPerMinute int
}
type statusInput struct {
	JobID string `json:"job_id"`
}
type statusOutput struct {
	JobID        string             `json:"job_id"`
	Status       string             `json:"status"`
	Error        string             `json:"error,omitempty"`
	Publications []jobs.Publication `json:"publications,omitempty"`
}
type queryInput struct {
	JobID     string `json:"job_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`
}
type regenerateInput struct {
	JobID     string `json:"job_id"`
	RequestID string `json:"request_id"`
}

func page(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 10000 {
		return 0, 0, errors.New("limit must be 1-100 and offset 0-10000")
	}
	return limit, offset, nil
}

func NewHandler(store catalog, options Options) http.Handler {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	transport := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		principal, _ := r.Context().Value(principalKey{}).(Credential)
		return scopedServer(store, principal, options)
	}, &sdk.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	if options.RequestsPerMinute <= 0 {
		options.RequestsPerMinute = 120
	}
	type window struct {
		start time.Time
		count int
	}
	windows := map[string]window{}
	var mu sync.Mutex
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		principal, ok := authenticateToken(r.Header.Get("Authorization"), options.Credentials)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gitlab-doc-platform-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		current := windows[principal.ID]
		if time.Since(current.start) >= time.Minute {
			current = window{start: time.Now()}
		}
		current.count++
		windows[principal.ID] = current
		mu.Unlock()
		if current.count > options.RequestsPerMinute {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "MCP request limit exceeded", http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser origins are not supported for this private MCP endpoint", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal))
		transport.ServeHTTP(w, r)
	}))
	return options.Metrics.Instrument("mcp", mux)
}

type principalKey struct{}

// NewManagementHandler must only be exposed on the private management listener.
func NewManagementHandler(options Options) http.Handler {
	mux := http.NewServeMux()
	if options.Metrics != nil {
		mux.Handle("GET /metrics", options.Metrics.Handler())
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if len(options.Credentials) == 0 || options.ReadyCheck != nil && options.ReadyCheck(ctx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func scopedServer(store catalog, principal Credential, options Options) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "gitlab-doc-platform", Version: "0.5.0"}, nil)
	load := func(ctx context.Context, input queryInput) (jobs.Job, error) {
		if input.JobID != "" {
			job, err := store.Get(ctx, input.JobID)
			if err != nil {
				return jobs.Job{}, err
			}
			if !allowed(principal, job.ProjectID) || input.ProjectID != "" && input.ProjectID != job.ProjectID {
				return jobs.Job{}, jobs.ErrNotFound
			}
			return job, nil
		}
		if !allowed(principal, input.ProjectID) {
			return jobs.Job{}, jobs.ErrNotFound
		}
		list, err := store.ListJobs(ctx, principal.ProjectIDs, input.ProjectID, 1, 0)
		if err != nil {
			return jobs.Job{}, err
		}
		if len(list) == 0 {
			return jobs.Job{}, jobs.ErrNotFound
		}
		return list[0], nil
	}
	safeError := func(err error) error {
		if errors.Is(err, jobs.ErrNotFound) {
			return errors.New("not found in authorized scope; a CI archive ingestion job is required")
		}
		options.Logger.Error("MCP store operation failed", "error", err)
		return errors.New("documentation store operation failed")
	}
	sdk.AddTool(server, &sdk.Tool{Name: "list_projects", Description: "List authorized projects with ingested source"}, func(ctx context.Context, req *sdk.CallToolRequest, in queryInput) (*sdk.CallToolResult, map[string]any, error) {
		limit, offset, err := page(in.Limit, in.Offset)
		if err != nil {
			return nil, nil, err
		}
		projects, err := store.ListProjects(ctx, principal.ProjectIDs, limit, offset)
		if err != nil {
			return nil, nil, safeError(err)
		}
		return nil, map[string]any{"projects": projects, "limit": limit, "offset": offset}, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "list_documentation_jobs", Description: "List jobs only within authorized projects"}, func(ctx context.Context, req *sdk.CallToolRequest, in queryInput) (*sdk.CallToolResult, map[string]any, error) {
		limit, offset, err := page(in.Limit, in.Offset)
		if err != nil {
			return nil, nil, err
		}
		if in.ProjectID != "" && !allowed(principal, in.ProjectID) {
			return nil, nil, safeError(jobs.ErrNotFound)
		}
		list, err := store.ListJobs(ctx, principal.ProjectIDs, in.ProjectID, limit, offset)
		if err != nil {
			return nil, nil, safeError(err)
		}
		for i := range list {
			list[i].ArchivePath = ""
			list[i].IdempotencyKey = ""
		}
		return nil, map[string]any{"jobs": list, "limit": limit, "offset": offset}, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "get_documentation_status", Description: "Read status of an authorized documentation job"}, func(ctx context.Context, req *sdk.CallToolRequest, in statusInput) (*sdk.CallToolResult, statusOutput, error) {
		job, err := load(ctx, queryInput{JobID: in.JobID})
		if errors.Is(err, jobs.ErrNotFound) {
			return nil, statusOutput{JobID: in.JobID, Status: "not_found", Error: "ingestion job is required before documentation can be queried"}, nil
		}
		if err != nil {
			return nil, statusOutput{}, safeError(err)
		}
		output := statusOutput{JobID: job.ID, Status: string(job.Status), Error: job.Error}
		if reader, ok := store.(interface {
			ListPublications(context.Context, string) ([]jobs.Publication, error)
		}); ok {
			output.Publications, err = reader.ListPublications(ctx, job.ID)
			if err != nil {
				return nil, statusOutput{}, safeError(err)
			}
		}
		return nil, output, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "get_application_summary", Description: "Read the persisted static summary of an authorized ingested repository"}, func(ctx context.Context, req *sdk.CallToolRequest, in queryInput) (*sdk.CallToolResult, map[string]any, error) {
		job, err := load(ctx, in)
		if err != nil {
			return nil, nil, safeError(err)
		}
		snapshot, err := store.GetAnalysis(ctx, job.ID)
		if err != nil {
			return nil, nil, safeError(err)
		}
		return nil, map[string]any{"job_id": job.ID, "project_id": snapshot.ProjectID, "commit_sha": snapshot.CommitSHA, "language": snapshot.Language, "framework": snapshot.Framework, "database_client": snapshot.Database, "evidence_items": len(snapshot.Evidence), "inventory_files": len(snapshot.Inventory), "warnings": snapshot.Warnings}, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "get_evidence", Description: "Read paginated persisted evidence without source contents"}, func(ctx context.Context, req *sdk.CallToolRequest, in queryInput) (*sdk.CallToolResult, map[string]any, error) {
		limit, offset, err := page(in.Limit, in.Offset)
		if err != nil {
			return nil, nil, err
		}
		job, err := load(ctx, in)
		if err != nil {
			return nil, nil, safeError(err)
		}
		snapshot, err := store.GetAnalysis(ctx, job.ID)
		if err != nil {
			return nil, nil, safeError(err)
		}
		start := offset
		if start > len(snapshot.Evidence) {
			start = len(snapshot.Evidence)
		}
		end := start + limit
		if end > len(snapshot.Evidence) {
			end = len(snapshot.Evidence)
		}
		return nil, map[string]any{"job_id": job.ID, "evidence": snapshot.Evidence[start:end], "total": len(snapshot.Evidence), "offset": offset}, nil
	})
	for _, tool := range []struct{ name, kind string }{{"get_technical_paper", "technical"}, {"get_infrastructure_paper", "infrastructure"}, {"get_application_architecture", "application_architecture"}} {
		name, kind := tool.name, tool.kind
		sdk.AddTool(server, &sdk.Tool{Name: name, Description: "Read a validated persisted document for an authorized project"}, func(ctx context.Context, req *sdk.CallToolRequest, in queryInput) (*sdk.CallToolResult, map[string]any, error) {
			job, err := load(ctx, in)
			if err != nil {
				return nil, nil, safeError(err)
			}
			if job.Status != jobs.Completed {
				return nil, map[string]any{"job_id": job.ID, "status": job.Status, "message": "documentation is not completed; poll the job status"}, nil
			}
			content, err := store.GetDocument(ctx, job.ID, kind)
			if errors.Is(err, jobs.ErrNotFound) {
				return nil, map[string]any{"job_id": job.ID, "message": "documents are not persisted for this legacy job; regenerate documentation or ingest a new source commit"}, nil
			}
			if err != nil {
				return nil, nil, safeError(err)
			}
			return nil, map[string]any{"job_id": job.ID, "project_id": job.ProjectID, "commit_sha": job.CommitSHA, "content": content}, nil
		})
	}
	sdk.AddTool(server, &sdk.Tool{Name: "regenerate_documentation", Description: "Queue generation from an already ingested archive; requires write scope and an idempotent request_id"}, func(ctx context.Context, req *sdk.CallToolRequest, in regenerateInput) (*sdk.CallToolResult, map[string]any, error) {
		if !principal.CanRegenerate {
			return nil, nil, errors.New("credential does not permit regeneration")
		}
		if in.RequestID == "" || len(in.RequestID) > 128 {
			return nil, nil, errors.New("request_id is required and limited to 128 bytes")
		}
		original, err := load(ctx, queryInput{JobID: in.JobID})
		if err != nil {
			return nil, nil, safeError(err)
		}
		if original.ArchivePath == "" {
			return nil, nil, safeError(jobs.ErrNotFound)
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, nil, errors.New("could not create generation identity")
		}
		job, created, err := store.CreateOrGet(ctx, jobs.Job{ID: hex.EncodeToString(random[:]), ProjectID: original.ProjectID, ProjectName: original.ProjectName, GroupID: original.GroupID, CommitSHA: original.CommitSHA, Branch: original.Branch, ArchivePath: original.ArchivePath, IdempotencyKey: fmt.Sprintf("mcp:%s:%s:%s:%s:%s", principal.ID, original.ID, evidence.DocumentationSchemaVersion, options.GenerationVersion, in.RequestID)})
		if err != nil {
			return nil, nil, safeError(err)
		}
		if created && options.Metrics != nil {
			options.Metrics.Jobs.WithLabelValues("accepted").Inc()
		}
		return nil, map[string]any{"job_id": job.ID, "status": job.Status, "created": created}, nil
	})
	return server
}
