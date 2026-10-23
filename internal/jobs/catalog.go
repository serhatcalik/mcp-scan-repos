package jobs

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
	"gitlab-doc-platform/internal/documents"
)

type Project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	GroupID string `json:"group_id"`
}

func (s *PostgresStore) ListJobs(ctx context.Context, projects []string, project string, limit, offset int) ([]Job, error) {
	rows, err := s.db.Query(ctx, `select id,project_id,project_name,group_id,commit_sha,branch,status,coalesce(error,''),created_at,updated_at from documentation_jobs where project_id=any($1::text[]) and ($2='' or project_id=$2) order by created_at desc,id desc limit $3 offset $4`, projects, project, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.ProjectID, &j.ProjectName, &j.GroupID, &j.CommitSHA, &j.Branch, &j.Status, &j.Error, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListProjects(ctx context.Context, projects []string, limit, offset int) ([]Project, error) {
	rows, err := s.db.Query(ctx, `select project_id,project_name,group_id from (select distinct on(project_id) project_id,project_name,group_id from documentation_jobs where project_id=any($1::text[]) order by project_id,created_at desc,id desc) p order by project_id limit $2 offset $3`, projects, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.GroupID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func documentContents(a documents.ArtifactSet) map[string]string {
	return map[string]string{"technical": a.TechnicalPaper, "infrastructure": a.InfrastructurePaper, "application_architecture": a.Diagrams["application-architecture.mmd"], "infrastructure_architecture": a.Diagrams["infrastructure-architecture.mmd"], "dependency_diagram": a.Diagrams["dependency-diagram.mmd"]}
}

func (s *PostgresStore) SaveDocuments(ctx context.Context, id string, a documents.ArtifactSet) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var found string
	if err := tx.QueryRow(ctx, `select id from documentation_jobs where id=$1 for update`, id).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.Exec(ctx, `delete from generated_documents where job_id=$1`, id); err != nil {
		return err
	}
	for kind, content := range documentContents(a) {
		if _, err := tx.Exec(ctx, `insert into generated_documents(job_id,document_type,content_markdown,validation_status) values($1,$2,$3,'passed')`, id, kind, content); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetDocument(ctx context.Context, id, kind string) (string, error) {
	var content string
	err := s.db.QueryRow(ctx, `select content_markdown from generated_documents where job_id=$1 and document_type=$2 and validation_status='passed' order by id desc limit 1`, id, kind).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return content, err
}

func (s *MemoryStore) SaveDocuments(ctx context.Context, id string, a documents.ArtifactSet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return ErrNotFound
	}
	s.documents[id] = documentContents(a)
	return nil
}

func (s *MemoryStore) GetDocument(ctx context.Context, id, kind string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.documents[id][kind]
	if !ok {
		return "", ErrNotFound
	}
	return content, nil
}

func (s *MemoryStore) ListJobs(ctx context.Context, projects []string, project string, limit, offset int) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed := map[string]bool{}
	for _, p := range projects {
		allowed[p] = true
	}
	out := []Job{}
	for _, j := range s.jobs {
		if allowed[j.ProjectID] && (project == "" || project == j.ProjectID) {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if offset >= len(out) {
		return []Job{}, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], nil
}

func (s *MemoryStore) ListProjects(ctx context.Context, projects []string, limit, offset int) ([]Project, error) {
	s.mu.Lock()
	total := len(s.jobs)
	s.mu.Unlock()
	all, err := s.ListJobs(ctx, projects, "", total+1, 0)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []Project{}
	for _, j := range all {
		if !seen[j.ProjectID] {
			out = append(out, Project{j.ProjectID, j.ProjectName, j.GroupID})
			seen[j.ProjectID] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if offset >= len(out) {
		return []Project{}, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], nil
}

func (s *PostgresStore) QueuedCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRow(ctx, `select count(*) from documentation_jobs where status='queued'`).Scan(&count)
	return count, err
}
