package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gitlab-doc-platform/internal/documents"
	publicationmeta "gitlab-doc-platform/internal/publication"
)

const publicationLock int64 = 734802620

type Runner struct {
	DB       *pgxpool.Pool
	Client   *Client
	Logger   *slog.Logger
	Interval time.Duration
}

// Configure persists only trusted non-secret destinations. Omitted targets are disabled.
// All replicas must use the same targets file during normal operation.
func Configure(ctx context.Context, db *pgxpool.Pool, targets []Target) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, publicationLock+1); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update notion_targets set enabled=false,updated_at=now()`); err != nil {
		return err
	}
	for _, target := range targets {
		if _, err := tx.Exec(ctx, `insert into notion_targets(group_id,parent_page_id,enabled) values($1,$2,$3)
   on conflict(group_id) do update set parent_page_id=excluded.parent_page_id,enabled=excluded.enabled,
   activated_at=case when notion_targets.parent_page_id<>excluded.parent_page_id then now() else notion_targets.activated_at end,
   updated_at=now()`, target.GroupID, target.ParentPageID, target.Enabled); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *Runner) Run(ctx context.Context) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	r.Logger.Info("Notion publisher started", "renderer", rendererVersion, "page_titles", "project-name/application-version-or-completion-time-utc")
	interval := r.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Error("Notion publication processing failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type publication struct {
	ID                                                          int64
	JobID, ProjectID, ProjectName, GroupID, CommitSHA, ParentID string
	Attempts                                                    int
	CreatedAt                                                   time.Time
	CompletedAt                                                 time.Time
	SnapshotLabel                                               string
}

// ProcessOne holds a dedicated PostgreSQL session lock through all remote calls.
// A running record is recoverable after process loss without a time-based lease
// allowing another replica to overwrite a still-running publication.
func (r *Runner) ProcessOne(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	conn, err := r.DB.Acquire(ctx)
	if err != nil {
		return errors.New("acquire Notion coordination connection failed")
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, publicationLock).Scan(&locked); err != nil {
		return errors.New("Notion coordination lock failed")
	}
	if !locked {
		return nil
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var unlocked bool
		if err := conn.QueryRow(cleanup, `select pg_advisory_unlock($1)`, publicationLock).Scan(&unlocked); err != nil || !unlocked {
			// Never return a session carrying an advisory lock to the connection pool.
			_ = conn.Hijack().Close(cleanup)
		}
	}()
	// Reconciliation of completed jobs makes the publication outbox resilient to
	// a crash between generation completion and enqueue. No historical backfill by default.
	if _, err := conn.Exec(ctx, `insert into publication_records(job_id,target,status)
  select j.id,'notion:'||t.parent_page_id,'queued' from documentation_jobs j
  join notion_targets t on t.group_id=j.group_id and t.enabled
  where j.status='completed' and j.updated_at>=t.activated_at
  on conflict(job_id,target) do nothing`); err != nil {
		return errors.New("enqueue Notion publications failed")
	}
	// Interrupted attempts count toward the bound too.
	if _, err := conn.Exec(ctx, `update publication_records set status='failed',updated_at=now(),
  details=details||'{"error":"publication retry limit reached"}'::jsonb
  where target like 'notion:%' and status in ('queued','retrying','publishing') and attempt_count>=10`); err != nil {
		return errors.New("expire Notion publication attempts failed")
	}
	var p publication
	err = conn.QueryRow(ctx, `select p.id,j.id,j.project_id,j.project_name,j.group_id,j.commit_sha,t.parent_page_id,p.attempt_count,j.created_at,j.updated_at
  from publication_records p join documentation_jobs j on j.id=p.job_id
  join notion_targets t on t.group_id=j.group_id and p.target='notion:'||t.parent_page_id
  where t.enabled and j.status='completed' and p.status in ('queued','retrying','publishing')
  and p.next_run_at<=now() and p.attempt_count<10
  and (select next_request_at<=now() from notion_publication_control where id=true)
  order by p.created_at,p.id limit 1`).Scan(&p.ID, &p.JobID, &p.ProjectID, &p.ProjectName, &p.GroupID, &p.CommitSHA, &p.ParentID, &p.Attempts, &p.CreatedAt, &p.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return errors.New("load Notion publication failed")
	}
	if _, err := conn.Exec(ctx, `update publication_records set status='publishing',attempt_count=attempt_count+1,next_run_at=now()+interval '1 minute',updated_at=now() where id=$1`, p.ID); err != nil {
		return errors.New("start Notion publication failed")
	}
	// Coordination connection is checked and a cross-replica request cooldown is
	// persisted before each HTTP call. Client is owned exclusively by this runner.
	r.Client.beforeRequest = func(ctx context.Context) error {
		var next time.Time
		if err := conn.QueryRow(ctx, `select next_request_at from notion_publication_control where id=true`).Scan(&next); err != nil {
			return errors.New("Notion coordination connection lost")
		}
		if delay := time.Until(next); delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		if _, err := conn.Exec(ctx, `update notion_publication_control set next_request_at=now()+interval '400 milliseconds' where id=true`); err != nil {
			return errors.New("Notion request coordination failed")
		}
		if _, err := conn.Exec(ctx, `update publication_records set next_run_at=now()+interval '1 minute' where id=$1`, p.ID); err != nil {
			return errors.New("Notion recovery delay persistence failed")
		}
		return nil
	}
	defer func() { r.Client.beforeRequest = nil }()
	details, publishErr := r.publish(ctx, conn, p)
	resultCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	status := "published"
	delay := time.Duration(0)
	if publishErr != nil {
		status = "retrying"
		delay = time.Minute * time.Duration(1<<min(p.Attempts, 6))
		var apiErr *APIError
		if errors.As(publishErr, &apiErr) {
			if !apiErr.Retryable() {
				status = "failed"
			}
			if apiErr.RetryAfter > delay {
				delay = apiErr.RetryAfter
			}
			if apiErr.Status == 429 || apiErr.Status == 529 {
				if _, err := conn.Exec(resultCtx, `update notion_publication_control set next_request_at=greatest(next_request_at,now()+$1*interval '1 second') where id=true`, delay.Seconds()); err != nil {
					return errors.New("persist Notion cooldown failed")
				}
			}
		}
		if p.Attempts+1 >= 10 {
			status = "failed"
		}
		// Client and publisher errors are deliberately safe, without raw response content.
		details["error"] = publishErr.Error()
	}
	data, err := json.Marshal(details)
	if err != nil {
		return errors.New("encode Notion publication result failed")
	}
	if _, err := conn.Exec(resultCtx, `update publication_records set status=$2,details=details||$3::jsonb,
  next_run_at=now()+$4*interval '1 second',updated_at=now() where id=$1`, p.ID, status, data, delay.Seconds()); err != nil {
		return errors.New("persist Notion publication result failed")
	}
	if r.Logger != nil {
		r.Logger.Info("Notion publication attempt finished", "job_id", p.JobID, "project_id", p.ProjectID, "group_id", p.GroupID, "status", status, "attempt", p.Attempts+1, "renderer", rendererVersion, "error", details["error"])
	}
	return nil
}

func (r *Runner) managedPage(ctx context.Context, conn *pgxpool.Conn, p publication, parent, job, kind, title string) (string, error) {
	identity, legacy := pageIdentity(p, kind)
	var id string
	err := conn.QueryRow(ctx, `select page_id from notion_pages where parent_page_id=$1 and project_id=$2 and job_id=$3 and kind=$4`, p.ParentID, p.ProjectID, job, kind).Scan(&id)
	if err == nil {
		if err := r.Client.renameLegacyPage(ctx, id, legacy, title); err != nil {
			return "", err
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("load managed Notion page failed")
	}
	id, err = r.Client.ensurePage(ctx, parent, title, identity, legacy)
	if err != nil {
		return "", err
	}
	if _, err := conn.Exec(ctx, `insert into notion_pages(parent_page_id,project_id,job_id,kind,page_id) values($1,$2,$3,$4,$5)`, p.ParentID, p.ProjectID, job, kind, id); err != nil {
		return "", errors.New("persist managed Notion page failed")
	}
	if err := r.Client.renameLegacyPage(ctx, id, legacy, title); err != nil {
		return "", err
	}
	return id, nil
}

func projectTitle(p publication) string {
	name := strings.Join(strings.Fields(documents.RedactText(p.ProjectName)), " ")
	if name == "" {
		return "GitLab Project " + p.ProjectID
	}
	runes := []rune(name)
	if len(runes) > richTextRunes {
		name = string(runes[:richTextRunes])
	}
	return name
}

func snapshotTitle(p publication) string {
	if p.SnapshotLabel != "" {
		return p.SnapshotLabel
	}
	date := p.CompletedAt
	if date.IsZero() {
		date = p.CreatedAt
	}
	return "Documentation - " + date.UTC().Format("2006-01-02 15:04:05 UTC")
}

func pageIdentity(p publication, kind string) (string, string) {
	switch kind {
	case "project":
		return "GitLab project ID: " + p.ProjectID, "GitLab Project " + p.ProjectID
	case "snapshot":
		return "Documentation job ID: " + p.JobID, "Documentation [" + p.JobID + "]"
	default:
		return "", ""
	}
}

func (r *Runner) publish(ctx context.Context, conn *pgxpool.Conn, p publication) (map[string]string, error) {
	details := map[string]string{"parent_url": PageURL(p.ParentID), "error": ""}
	// Fetch only validated generated documents. No uploaded source or token enters Notion.
	rows, err := conn.Query(ctx, `select document_type,content_markdown from generated_documents where job_id=$1 and validation_status='passed' order by id`, p.JobID)
	if err != nil {
		return details, errors.New("load documents for Notion failed")
	}
	contents := map[string]string{}
	for rows.Next() {
		var kind, content string
		if err := rows.Scan(&kind, &content); err != nil {
			rows.Close()
			return details, errors.New("read documents for Notion failed")
		}
		contents[kind] = content
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return details, errors.New("read documents for Notion failed")
	}
	kinds := []struct {
		key, title string
		code       bool
	}{
		{"technical", "Technical Paper", false}, {"infrastructure", "Infrastructure Paper", false},
		{"application_architecture", "Application Architecture", true}, {"infrastructure_architecture", "Infrastructure Architecture", true}, {"dependency_diagram", "Dependency Diagram", true},
	}
	desired := map[string][]block{}
	for _, kind := range kinds {
		content, ok := contents[kind.key]
		if !ok || content == "" {
			return details, errors.New("required validated Notion document is unavailable")
		}
		if documents.RedactText(content) != content {
			return details, errors.New("sensitive pattern detected in publication content")
		}
		desired[kind.key], err = MarkdownBlocks(content, kind.code)
		if err != nil {
			return details, err
		}
	}
	p.SnapshotLabel, err = publicationmeta.SnapshotLabel(ctx, conn, p.ID, p.JobID, p.ProjectID, "notion:"+p.ParentID, snapshotTitle(p), "")
	if err != nil {
		return details, err
	}
	project, err := r.managedPage(ctx, conn, p, p.ParentID, "", "project", projectTitle(p))
	if err != nil {
		return details, err
	}
	details["project_url"] = PageURL(project)
	snapshot, err := r.managedPage(ctx, conn, p, project, p.JobID, "snapshot", snapshotTitle(p))
	if err != nil {
		return details, err
	}
	details["snapshot_url"] = PageURL(snapshot)
	info, err := MarkdownBlocks(fmt.Sprintf("# %s\nGitLab project: %s\nCommit: %s\nBranch: main\nJob: %s\nIngested: %s\nGenerated papers retain verified, inferred and unavailable information.\n", documents.RedactText(p.ProjectName), p.ProjectID, p.CommitSHA, p.JobID, p.CreatedAt.UTC().Format(time.RFC3339)), false)
	if err != nil {
		return details, err
	}
	// Snapshot contains only its immutable metadata followed by managed child pages.
	// Write metadata once before creating children, then do not overwrite the page.
	var initialized bool
	if err := conn.QueryRow(ctx, `select coalesce((details->>'snapshot_initialized')::boolean,false) from publication_records where id=$1`, p.ID).Scan(&initialized); err != nil {
		return details, errors.New("load Notion snapshot initialization failed")
	}
	if !initialized {
		actual, err := r.Client.children(ctx, snapshot)
		if err != nil {
			return details, err
		}
		identity, _ := pageIdentity(p, "snapshot")
		// New snapshots begin with an identity marker; old ones keep their original prefix.
		if len(actual) > 0 && actual[0].Type == "paragraph" && actual[0].text() == identity {
			info = append([]block{textBlock("paragraph", identity)}, info...)
		}
		if err := r.Client.EnsureContent(ctx, snapshot, info); err != nil {
			return details, err
		}
		if _, err := conn.Exec(ctx, `update publication_records set details=details||'{"snapshot_initialized":true}'::jsonb where id=$1`, p.ID); err != nil {
			return details, errors.New("persist Notion snapshot initialization failed")
		}
	}
	for _, kind := range kinds {
		page, err := r.managedPage(ctx, conn, p, snapshot, p.JobID, kind.key, kind.title)
		if err != nil {
			return details, err
		}
		if err := r.Client.EnsureContent(ctx, page, desired[kind.key]); err != nil {
			return details, err
		}
	}
	return details, nil
}
