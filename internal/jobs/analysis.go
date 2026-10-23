package jobs

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"gitlab-doc-platform/internal/evidence"
)

func (s *PostgresStore) SaveAnalysis(ctx context.Context, id string, snapshot evidence.Snapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var project, sha string
	if err := tx.QueryRow(ctx, `select project_id,commit_sha from documentation_jobs where id=$1 for update`, id).Scan(&project, &sha); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if project != snapshot.ProjectID || sha != snapshot.CommitSHA {
		return ErrSnapshotIdentity
	}
	if _, err := tx.Exec(ctx, `insert into analysis_snapshots(job_id,project_id,commit_sha,snapshot) values($1,$2,$3,$4)`, id, project, sha, data); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from evidence_items where job_id=$1`, id); err != nil {
		return err
	}
	rows := make([][]any, 0, len(snapshot.Evidence))
	for _, e := range snapshot.Evidence {
		related := e.RelatedComponents
		if related == nil {
			related = []string{}
		}
		// The original evidence ID remains in JSON; SQL IDs are scoped to a job.
		rows = append(rows, []any{id + ":" + e.ID, id, project, sha, e.Category, e.Type, e.Value, e.SourceFile, e.LineStart, e.LineEnd, e.Verification, string(e.Confidence), related})
	}
	if len(rows) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"evidence_items"}, []string{"id", "job_id", "project_id", "commit_sha", "category", "type", "value", "source_file", "line_start", "line_end", "verification", "confidence", "related_components"}, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetAnalysis(ctx context.Context, id string) (evidence.Snapshot, error) {
	var data []byte
	if err := s.db.QueryRow(ctx, `select snapshot from analysis_snapshots where job_id=$1 order by id desc limit 1`, id).Scan(&data); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evidence.Snapshot{}, ErrNotFound
		}
		return evidence.Snapshot{}, err
	}
	var snapshot evidence.Snapshot
	err := json.Unmarshal(data, &snapshot)
	return snapshot, err
}
