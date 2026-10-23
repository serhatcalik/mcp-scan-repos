package confluence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gitlab-doc-platform/internal/documents"
	publicationmeta "gitlab-doc-platform/internal/publication"
)

const publicationLock int64 = 734802622

type Runner struct {
	DB       *pgxpool.Pool
	Client   *Client
	Logger   *slog.Logger
	Interval time.Duration
}

func Configure(ctx context.Context, db *pgxpool.Pool, settings Settings) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, publicationLock+1); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update confluence_targets set enabled=false,updated_at=now()`); err != nil {
		return err
	}
	for _, t := range settings.Targets {
		if _, err := tx.Exec(ctx, `insert into confluence_targets(group_id,destination_key,space_id,space_key,parent_page_id,enabled)
values($1,$2,$3,$4,$5,$6) on conflict(group_id) do update set
destination_key=excluded.destination_key,space_id=excluded.space_id,space_key=excluded.space_key,
parent_page_id=excluded.parent_page_id,enabled=excluded.enabled,
activated_at=case when confluence_targets.destination_key<>excluded.destination_key then now() else confluence_targets.activated_at end,
updated_at=now()`, t.GroupID, settings.destination(t), t.SpaceID, t.SpaceKey, t.ParentPageID, t.Enabled); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *Runner) Run(ctx context.Context) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	r.Logger.Info("Confluence publisher started", "renderer", rendererVersion, "edition", r.Client.settings.Edition, "mermaid_app", r.Client.settings.MermaidApp, "mermaid_macro", r.Client.settings.MermaidMacro)
	interval := r.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Error("Confluence publication processing failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type publication struct {
	DocumentTitleStyle                                             string
	Title                                                          string
	SnapshotLabel                                                  string
	ID                                                             int64
	JobID, ProjectID, ProjectName, GroupID, CommitSHA, Destination string
	Target                                                         Target
	Attempts                                                       int
	CompletedAt                                                    time.Time
}

// A dedicated session lock prevents concurrent remote writes and survives slow
// HTTP calls without a lease expiry handing a live publication to another worker.
func (r *Runner) ProcessOne(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	conn, err := r.DB.Acquire(ctx)
	if err != nil {
		return errors.New("acquire Confluence coordination connection failed")
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, publicationLock).Scan(&locked); err != nil {
		return errors.New("Confluence coordination lock failed")
	}
	if !locked {
		return nil
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var unlocked bool
		if err := conn.QueryRow(cleanup, `select pg_advisory_unlock($1)`, publicationLock).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Hijack().Close(cleanup)
		}
	}()
	if _, err := conn.Exec(ctx, `insert into publication_records(job_id,target,status)
select j.id,'confluence:'||t.destination_key,'queued' from documentation_jobs j
join confluence_targets t on t.group_id=j.group_id and t.enabled
where j.status='completed' and j.updated_at>=t.activated_at
on conflict(job_id,target) do nothing`); err != nil {
		return errors.New("enqueue Confluence publications failed")
	}
	if _, err := conn.Exec(ctx, `update publication_records set status='failed',updated_at=now(),details=details||'{"error":"publication retry limit reached"}'::jsonb
where target like 'confluence:%' and status in ('queued','retrying','publishing') and attempt_count>=10`); err != nil {
		return errors.New("expire Confluence attempts failed")
	}
	var p publication
	err = conn.QueryRow(ctx, `select p.id,j.id,j.project_id,j.project_name,j.group_id,j.commit_sha,t.destination_key,t.space_id,t.space_key,t.parent_page_id,p.attempt_count,j.updated_at
from publication_records p join documentation_jobs j on j.id=p.job_id
join confluence_targets t on t.group_id=j.group_id and p.target='confluence:'||t.destination_key
where t.enabled and j.status='completed' and p.status in ('queued','retrying','publishing') and p.next_run_at<=now() and p.attempt_count<10
and (select next_request_at<=now() from confluence_publication_control where id=true)
order by p.created_at,p.id limit 1`).Scan(&p.ID, &p.JobID, &p.ProjectID, &p.ProjectName, &p.GroupID, &p.CommitSHA, &p.Destination, &p.Target.SpaceID, &p.Target.SpaceKey, &p.Target.ParentPageID, &p.Attempts, &p.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return errors.New("load Confluence publication failed")
	}
	if _, err := conn.Exec(ctx, `update publication_records set status='publishing',attempt_count=attempt_count+1,next_run_at=now()+interval '1 minute',updated_at=now() where id=$1`, p.ID); err != nil {
		return errors.New("start Confluence publication failed")
	}
	r.Client.beforeRequest = func(ctx context.Context) error {
		var next time.Time
		if err := conn.QueryRow(ctx, `select next_request_at from confluence_publication_control where id=true`).Scan(&next); err != nil {
			return errors.New("Confluence coordination connection lost")
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
		if _, err := conn.Exec(ctx, `update confluence_publication_control set next_request_at=now()+interval '400 milliseconds' where id=true`); err != nil {
			return errors.New("Confluence cooldown persistence failed")
		}
		if _, err := conn.Exec(ctx, `update publication_records set next_run_at=now()+interval '1 minute' where id=$1`, p.ID); err != nil {
			return errors.New("Confluence recovery delay persistence failed")
		}
		return nil
	}
	defer func() { r.Client.beforeRequest = nil }()
	details, publishErr := r.publish(ctx, conn, p)
	status, delay := publicationResult(p.Attempts, publishErr)
	if publishErr != nil {
		details["error"] = publishErr.Error()
	}
	resultCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var apiErr *APIError
	if errors.As(publishErr, &apiErr) && apiErr.Status == 429 {
		if _, err := conn.Exec(resultCtx, `update confluence_publication_control set next_request_at=greatest(next_request_at,now()+$1*interval '1 second') where id=true`, delay.Seconds()); err != nil {
			return errors.New("persist Confluence cooldown failed")
		}
	}
	data, err := json.Marshal(details)
	if err != nil {
		return errors.New("encode Confluence result failed")
	}
	if _, err := conn.Exec(resultCtx, `update publication_records set status=$2,details=details||$3::jsonb,next_run_at=now()+$4*interval '1 second',updated_at=now() where id=$1`, p.ID, status, data, delay.Seconds()); err != nil {
		return errors.New("persist Confluence result failed")
	}
	if r.Logger != nil {
		r.Logger.Info("Confluence publication attempt finished", "job_id", p.JobID, "project_id", p.ProjectID, "group_id", p.GroupID, "status", status, "attempt", p.Attempts+1, "renderer", rendererVersion, "error", details["error"])
	}
	return nil
}

type permanentError struct{ message string }

func (e *permanentError) Error() string { return e.message }

func publicationResult(attempts int, err error) (string, time.Duration) {
	if err == nil {
		return "published", 0
	}
	status := "retrying"
	delay := time.Minute * time.Duration(1<<min(attempts, 6))
	var apiErr *APIError
	var permanent *permanentError
	if errors.As(err, &permanent) {
		status = "failed"
	}
	if errors.As(err, &apiErr) {
		if !apiErr.Retryable() {
			status = "failed"
		}
		delay = max(delay, apiErr.RetryAfter)
	}
	if attempts+1 >= 10 {
		status = "failed"
	}
	return status, delay
}

func (r *Runner) managedPage(ctx context.Context, conn *pgxpool.Conn, p publication, parent, job, kind, title, body string) (string, error) {
	var mapped string
	err := conn.QueryRow(ctx, `select page_id from confluence_pages where destination_key=$1 and project_id=$2 and job_id=$3 and kind=$4`, p.Destination, p.ProjectID, job, kind).Scan(&mapped)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("load managed Confluence page failed")
	}
	id, err := r.Client.EnsurePage(ctx, p.Target, parent, title, body, mapped)
	if err != nil {
		return "", fmt.Errorf("Confluence %s page: %w", kind, err)
	}
	if mapped == "" {
		if _, err := conn.Exec(ctx, `insert into confluence_pages(destination_key,project_id,job_id,kind,page_id) values($1,$2,$3,$4,$5)`, p.Destination, p.ProjectID, job, kind, id); err != nil {
			return "", errors.New("persist managed Confluence page failed")
		}
	}
	return id, nil
}

func projectTitle(p publication) string {
	name := strings.Join(strings.Fields(documents.RedactText(p.ProjectName)), " ")
	if name == "" {
		return "GitLab Project " + p.ProjectID
	}
	runes := []rune(name)
	return string(runes[:min(len(runes), 100)])
}

func snapshotTitle(p publication) string {
	if p.Title != "" {
		return p.Title
	}
	if p.SnapshotLabel != "" {
		return p.SnapshotLabel
	}
	return "Documentation - " + p.CompletedAt.UTC().Format("2006-01-02 15:04:05.999999 UTC")
}

func documentTitle(p publication, kind string) string {
	if p.DocumentTitleStyle == "kind-first" {
		return kind + " - " + strings.TrimPrefix(snapshotTitle(p), "Documentation - ")
	}
	return snapshotTitle(p) + " - " + kind
}

func freezeSnapshotTitle(ctx context.Context, conn *pgxpool.Conn, p publication) (string, error) {
	var title, scope string
	if err := conn.QueryRow(ctx, `select coalesce(details->>'snapshot_title',''),coalesce(details->>'snapshot_title_scope','') from publication_records where id=$1`, p.ID).Scan(&title, &scope); err != nil {
		return "", errors.New("load Confluence snapshot title failed")
	}
	if title != "" {
		if len([]rune(title)) > 255 || strings.ContainsAny(title, "\r\n") || documents.RedactText(title) != title {
			return "", errors.New("invalid stored Confluence snapshot title")
		}
		return title, nil
	}
	title = snapshotTitle(p)
	// Records without the new space scope used the old project-prefixed titles.
	// Keep that format even after a lost create response or incomplete mapping.
	if scope == "" {
		title = projectTitle(p) + " - " + title
	}
	style := "snapshot-first"
	if scope != "" {
		style = "kind-first"
	}
	if _, err := conn.Exec(ctx, `update publication_records set details=details||jsonb_build_object('snapshot_title',$2::text,'document_title_style',$3::text),updated_at=now() where id=$1`, p.ID, title, style); err != nil {
		return "", errors.New("persist Confluence snapshot title failed")
	}
	return title, nil
}

func (r *Runner) publish(ctx context.Context, conn *pgxpool.Conn, p publication) (map[string]string, error) {
	details := map[string]string{"parent_url": r.Client.PageURL(p.Target.ParentPageID), "error": "", "renderer": rendererVersion}
	rows, err := conn.Query(ctx, `select document_type,content_markdown from generated_documents where job_id=$1 and validation_status='passed' order by id`, p.JobID)
	if err != nil {
		return details, errors.New("load Confluence documents failed")
	}
	contents := map[string]string{}
	for rows.Next() {
		var kind, content string
		if err := rows.Scan(&kind, &content); err != nil {
			rows.Close()
			return details, errors.New("read Confluence documents failed")
		}
		contents[kind] = content
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return details, errors.New("read Confluence documents failed")
	}
	kinds := []struct {
		key, title string
		diagram    bool
	}{
		{"technical", "Technical Paper", false}, {"infrastructure", "Infrastructure Paper", false},
		{"application_architecture", "Application Architecture", true}, {"infrastructure_architecture", "Infrastructure Architecture", true}, {"dependency_diagram", "Dependency Diagram", true},
	}
	bodies := map[string]string{}
	for _, kind := range kinds {
		content := contents[kind.key]
		if content == "" {
			return details, &permanentError{"required validated Confluence document is unavailable"}
		}
		if documents.RedactText(content) != content {
			return details, &permanentError{"sensitive pattern detected in publication content"}
		}
		body, err := MarkdownStorage(content, kind.diagram, r.Client.settings)
		if err != nil {
			return details, &permanentError{err.Error()}
		}
		bodies[kind.key] = `<p>Documentation job ID: ` + html.EscapeString(p.JobID) + `</p>` + body
	}
	dateLabel := "Documentation - " + p.CompletedAt.UTC().Format("2006-01-02 15:04:05.999999 UTC")
	p.SnapshotLabel, err = publicationmeta.SnapshotLabel(ctx, conn, p.ID, p.JobID, p.ProjectID, "confluence:"+p.Destination, dateLabel, r.Client.settings.titleScope(p.Target))
	if err != nil {
		return details, err
	}
	p.Title, err = freezeSnapshotTitle(ctx, conn, p)
	if err != nil {
		return details, err
	}
	if err := conn.QueryRow(ctx, `select coalesce(details->>'document_title_style','snapshot-first') from publication_records where id=$1`, p.ID).Scan(&p.DocumentTitleStyle); err != nil {
		return details, errors.New("load Confluence document title style failed")
	}
	project, err := r.managedPage(ctx, conn, p, p.Target.ParentPageID, "", "project", projectTitle(p), `<p>GitLab project ID: `+html.EscapeString(p.ProjectID)+`</p>`)
	if err != nil {
		return details, err
	}
	details["project_url"] = r.Client.PageURL(project)
	info := `<p>Documentation job ID: ` + html.EscapeString(p.JobID) + `</p><p>Commit: ` + html.EscapeString(p.CommitSHA) + `</p><p>Branch: main</p>`
	snapshot, err := r.managedPage(ctx, conn, p, project, p.JobID, "snapshot", snapshotTitle(p), info)
	if err != nil {
		return details, err
	}
	details["snapshot_url"] = r.Client.PageURL(snapshot)
	for _, kind := range kinds {
		if _, err := r.managedPage(ctx, conn, p, snapshot, p.JobID, kind.key, documentTitle(p, kind.title), bodies[kind.key]); err != nil {
			return details, err
		}
	}
	return details, nil
}
