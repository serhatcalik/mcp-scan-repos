package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Publication struct {
	Target      string    `json:"target"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	ParentURL   string    `json:"parent_url,omitempty"`
	ProjectURL  string    `json:"project_url,omitempty"`
	SnapshotURL string    `json:"snapshot_url,omitempty"`
	Error       string    `json:"error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Callers must authorize the job before reading its publication destinations.
func (s *PostgresStore) ListPublications(ctx context.Context, jobID string) ([]Publication, error) {
	rows, err := s.db.Query(ctx, `select target,status,attempt_count,details,updated_at from publication_records where job_id=$1 order by id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Publication{}
	for rows.Next() {
		var p Publication
		var raw []byte
		if err := rows.Scan(&p.Target, &p.Status, &p.Attempts, &raw, &p.UpdatedAt); err != nil {
			return nil, err
		}
		var details struct {
			ParentURL   string `json:"parent_url"`
			ProjectURL  string `json:"project_url"`
			SnapshotURL string `json:"snapshot_url"`
			Error       string `json:"error"`
		}
		if json.Unmarshal(raw, &details) != nil {
			return nil, errors.New("invalid publication details")
		}
		p.ParentURL, p.ProjectURL, p.SnapshotURL, p.Error = details.ParentURL, details.ProjectURL, details.SnapshotURL, details.Error
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *MemoryStore) ListPublications(ctx context.Context, jobID string) ([]Publication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []Publication{}, nil
}
