package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitlab-doc-platform/internal/auth"
	"gitlab-doc-platform/internal/config"
	"gitlab-doc-platform/internal/evidence"
	"gitlab-doc-platform/internal/jobs"
	"gitlab-doc-platform/internal/observability"
)

type Dependencies struct {
	Config     config.Config
	Jobs       jobs.Store
	Logger     *slog.Logger
	ReadyCheck func(context.Context) error
	Metrics    *observability.Metrics
}

func NewRouter(deps Dependencies) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(requestLogger(deps.Logger, deps.Metrics))
	r.Use(gin.Recovery())
	r.Use(requestSizeLimit(deps.Config.MaxArchiveBytes + (1 << 20)))
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		if deps.ReadyCheck != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			if err := deps.ReadyCheck(ctx); err != nil {
				logFailure(c, deps.Logger, "database readiness check failed", err)
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not ready"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	r.GET("/metrics", func(c *gin.Context) {
		if deps.Metrics == nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		deps.Metrics.Handler().ServeHTTP(c.Writer, c.Request)
	})
	v := r.Group("/api/v1")
	validator := &auth.Validator{
		Issuer: deps.Config.GitLabIssuer, Audience: deps.Config.GitLabAudience, JWKSURL: deps.Config.GitLabJWKSURL,
		AllowedGroups: deps.Config.AllowedGroups, AllowedProjects: deps.Config.AllowedProjects,
		RequireProtectedRef: deps.Config.RequireProtectedRef,
	}
	v.Use(authenticate(deps, validator))
	v.Use(projectRateLimit(deps.Config.APIRequestsPerMinute))
	v.POST("/jobs", createJob(deps))
	v.GET("/jobs/:id", getJob(deps))
	v.GET("/jobs/:id/artifacts", artifacts(deps))
	v.GET("/jobs/:id/publications", publications(deps))
	if deps.Config.GitHubEnabled {
		github := r.Group("/api/v1/github")
		github.Use(authenticate(deps, &auth.Validator{
			Provider: "github", Issuer: deps.Config.GitHubIssuer, Audience: deps.Config.GitHubAudience,
			JWKSURL: deps.Config.GitHubJWKSURL, AllowedGroups: deps.Config.AllowedGitHubOwners,
			AllowedProjects: deps.Config.AllowedGitHubRepos, RequireProtectedRef: deps.Config.GitHubProtectedRef,
		}))
		github.Use(projectRateLimit(deps.Config.APIRequestsPerMinute))
		github.POST("/jobs", createJob(deps))
		github.GET("/jobs/:id", getJob(deps))
		github.GET("/jobs/:id/artifacts", artifacts(deps))
		github.GET("/jobs/:id/publications", publications(deps))
	}
	return r
}

func publications(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := deps.Jobs.Get(c.Request.Context(), c.Param("id"))
		if errors.Is(err, jobs.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load job"})
			return
		}
		if !ownsJob(c, job) {
			return
		}
		reader, ok := deps.Jobs.(interface {
			ListPublications(context.Context, string) ([]jobs.Publication, error)
		})
		if !ok {
			c.JSON(http.StatusOK, gin.H{"job_id": job.ID, "publications": []jobs.Publication{}})
			return
		}
		items, err := reader.ListPublications(c.Request.Context(), job.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load publications"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"job_id": job.ID, "publications": items})
	}
}

func authenticate(deps Dependencies, validator *auth.Validator) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearer(c.GetHeader("Authorization"))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := validator.Validate(c.Request.Context(), token)
		if err != nil {
			deps.Logger.WarnContext(c.Request.Context(), "oidc validation failed", "request_id", c.GetString("request_id"), "error", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid oidc token"})
			return
		}
		c.Set("oidc_claims", claims)
		c.Next()
	}
}

func createJob(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := c.MustGet("oidc_claims").(auth.Claims)
		if err := c.Request.ParseMultipartForm(deps.Config.MaxArchiveBytes); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart request"})
			return
		}
		if c.Request.MultipartForm != nil {
			defer c.Request.MultipartForm.RemoveAll()
		}
		if clientSHA := c.PostForm("commit_sha"); clientSHA != "" && claims.SHA != "" && clientSHA != claims.SHA {
			c.JSON(http.StatusBadRequest, gin.H{"error": "commit sha does not match oidc claims"})
			return
		}
		if project := c.PostForm("project_id"); project != "" && project != claims.UploadProjectID() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "project id does not match oidc claims"})
			return
		}
		if branch := c.PostForm("branch"); branch != "" && branch != claims.Ref {
			c.JSON(http.StatusBadRequest, gin.H{"error": "branch does not match oidc claims"})
			return
		}
		file, header, err := c.Request.FormFile("archive")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "archive field is required"})
			return
		}
		defer file.Close()
		if header.Size > deps.Config.MaxArchiveBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "archive too large"})
			return
		}
		id := newID()
		archivePath := filepath.Join(deps.Config.StorageDir, "archives", id+".tar.gz")
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o750); err != nil {
			logFailure(c, deps.Logger, "failed to prepare archive storage", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare archive storage"})
			return
		}
		out, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			logFailure(c, deps.Logger, "failed to create archive", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create archive"})
			return
		}
		storedBytes, copyErr := io.Copy(out, file)
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			logFailure(c, deps.Logger, "failed to store archive", errors.Join(copyErr, closeErr))
			_ = os.Remove(archivePath)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store archive"})
			return
		}
		if deps.Metrics != nil {
			deps.Metrics.IngestBytes.Add(float64(storedBytes))
		}
		key := fmt.Sprintf("%s:%s:schema-%s:config-%s", claims.ProjectID, claims.SHA, evidence.DocumentationSchemaVersion, deps.Config.GenerationVersion())
		job, created, err := deps.Jobs.CreateOrGet(c.Request.Context(), jobs.Job{
			ID: id, ProjectID: claims.ProjectID, ProjectName: claims.ProjectPath, GroupID: claims.NamespaceID,
			GroupName: claims.Namespace,
			CommitSHA: claims.SHA, Branch: claims.Ref, IdempotencyKey: key, ArchivePath: archivePath,
		})
		if !created {
			_ = os.Remove(archivePath)
		}
		if err != nil {
			_ = os.Remove(archivePath)
			if errors.Is(err, jobs.ErrGroupDisabled) || errors.Is(err, jobs.ErrProjectNotAllowed) {
				c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
				return
			}
			logFailure(c, deps.Logger, "failed to create job", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create job"})
			return
		}
		status := http.StatusAccepted
		if !created {
			status = http.StatusOK
		}
		c.JSON(status, gin.H{"job_id": job.ID, "status": job.Status, "created": created})
		if created && deps.Metrics != nil {
			deps.Metrics.Jobs.WithLabelValues("accepted").Inc()
		}
		deps.Logger.InfoContext(c.Request.Context(), "documentation job accepted", "request_id", c.GetString("request_id"), "job_id", job.ID, "project_id", job.ProjectID, "group_id", job.GroupID, "created", created, "status", job.Status)
	}
}

func getJob(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := deps.Jobs.Get(c.Request.Context(), c.Param("id"))
		if errors.Is(err, jobs.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		if err != nil {
			logFailure(c, deps.Logger, "failed to load job", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load job"})
			return
		}
		if !ownsJob(c, job) {
			return
		}
		c.JSON(http.StatusOK, job)
	}
}

func artifacts(deps Dependencies) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := deps.Jobs.Get(c.Request.Context(), c.Param("id"))
		if errors.Is(err, jobs.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		if err != nil {
			logFailure(c, deps.Logger, "failed to load job", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load job"})
			return
		}
		if !ownsJob(c, job) {
			return
		}
		if job.Status != jobs.Completed {
			c.JSON(http.StatusConflict, gin.H{"error": "artifacts are not ready", "status": job.Status})
			return
		}
		path := filepath.Join(deps.Config.StorageDir, "artifacts", job.ID+".tar.gz")
		if _, err := os.Stat(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logFailure(c, deps.Logger, "failed to access artifact bundle", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to access artifact bundle"})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "artifact bundle not found"})
			return
		}
		c.FileAttachment(path, "generated-docs.tar.gz")
	}
}

func ownsJob(c *gin.Context, job jobs.Job) bool {
	claims := c.MustGet("oidc_claims").(auth.Claims)
	if claims.ProjectID != job.ProjectID || claims.NamespaceID != job.GroupID {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return false
	}
	return true
}

func requestLogger(logger *slog.Logger, metrics *observability.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		requestID := newID()
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
		metrics.HTTP("api", c.Writer.Status())
		level := slog.LevelInfo
		if c.Writer.Status() >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		logger.Log(c.Request.Context(), level, "http request", "request_id", requestID,
			"method", c.Request.Method, "route", c.FullPath(), "status", c.Writer.Status(), "duration", time.Since(started))
	}
}

func logFailure(c *gin.Context, logger *slog.Logger, message string, err error) {
	logger.ErrorContext(c.Request.Context(), message, "request_id", c.GetString("request_id"), "error", err)
}

func requestSizeLimit(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		c.Next()
	}
}

func bearer(header string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(header, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(header, prefix))
	}
	return ""
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
