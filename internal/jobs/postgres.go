package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	db *pgxpool.Pool
}

func NewPostgresStore(db *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) CreateOrGet(ctx context.Context, job Job) (Job, bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback(context.Background())
	name := job.GroupName
	if name == "" {
		name = job.GroupID
	}
	// Only ingestion with already-validated CI claims creates group records.
	if _, err := tx.Exec(ctx, `insert into gitlab_groups(id, name) values($1, $2) on conflict(id) do nothing`, job.GroupID, name); err != nil {
		return Job{}, false, err
	}
	var enabled, allowed bool
	if err := tx.QueryRow(ctx, `select enabled, cardinality(allowed_projects)=0 or $2=any(allowed_projects) from gitlab_groups where id=$1 for share`, job.GroupID, job.ProjectID).Scan(&enabled, &allowed); err != nil {
		return Job{}, false, err
	}
	if !enabled {
		return Job{}, false, ErrGroupDisabled
	}
	if !allowed {
		return Job{}, false, ErrProjectNotAllowed
	}
	const q = `
insert into documentation_jobs (id, project_id, project_name, group_id, commit_sha, branch, idempotency_key, archive_path, status)
values ($1,$2,$3,$4,$5,$6,$7,$8,'queued')
on conflict (idempotency_key) do update set
  status = case when documentation_jobs.status='failed' then 'queued' else documentation_jobs.status end,
  error = case when documentation_jobs.status='failed' then null else documentation_jobs.error end,
  attempt_count = case when documentation_jobs.status='failed' then 0 else documentation_jobs.attempt_count end,
  next_run_at = case when documentation_jobs.status='failed' then null else documentation_jobs.next_run_at end,
  lease_expires_at = case when documentation_jobs.status='failed' then null else documentation_jobs.lease_expires_at end,
  updated_at = case when documentation_jobs.status='failed' then now() else documentation_jobs.updated_at end
returning id, project_id, project_name, group_id, commit_sha, branch, idempotency_key, archive_path, status, coalesce(error,''), created_at, updated_at,
  (xmax = 0) as inserted`
	var inserted bool
	err = tx.QueryRow(ctx, q, job.ID, job.ProjectID, job.ProjectName, job.GroupID, job.CommitSHA, job.Branch, job.IdempotencyKey, job.ArchivePath).
		Scan(&job.ID, &job.ProjectID, &job.ProjectName, &job.GroupID, &job.CommitSHA, &job.Branch, &job.IdempotencyKey, &job.ArchivePath, &job.Status, &job.Error, &job.CreatedAt, &job.UpdatedAt, &inserted)
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, err
	}
	return job, inserted, nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (Job, error) {
	const q = `select id, project_id, project_name, group_id, commit_sha, branch, idempotency_key, archive_path, status, coalesce(error,''), created_at, updated_at from documentation_jobs where id=$1`
	var job Job
	err := s.db.QueryRow(ctx, q, id).Scan(&job.ID, &job.ProjectID, &job.ProjectName, &job.GroupID, &job.CommitSHA, &job.Branch, &job.IdempotencyKey, &job.ArchivePath, &job.Status, &job.Error, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

func (s *PostgresStore) LeaseNext(ctx context.Context, groups []string, leaseFor time.Duration) (Job, bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `update documentation_jobs set status='failed', error='worker lease expired; retry limit reached', lease_expires_at=null, updated_at=now()
where status in ('analyzing','generating','validating') and (lease_expires_at is null or lease_expires_at <= now()) and attempt_count >= max_attempts`); err != nil {
		return Job{}, false, err
	}
	// Lock a group before counting active jobs so concurrent workers obey its limit.
	const selectGroup = `select g.id from gitlab_groups g
where g.enabled and (cardinality($1::text[])=0 or g.id=any($1::text[]))
  and exists(select 1 from documentation_jobs j where j.group_id=g.id
    and j.attempt_count < j.max_attempts
    and (j.next_run_at is null or j.next_run_at <= now())
    and (j.status='queued' or (j.status in ('analyzing','generating','validating') and (j.lease_expires_at is null or j.lease_expires_at <= now()))))
  and (select count(*) from documentation_jobs active where active.group_id=g.id
    and active.status in ('analyzing','generating','validating') and active.lease_expires_at > now()) < g.max_concurrent_jobs
order by g.id for update of g skip locked limit 1`
	if groups == nil {
		groups = []string{}
	}
	var groupID string
	if err := tx.QueryRow(ctx, selectGroup, groups).Scan(&groupID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, false, tx.Commit(ctx)
		}
		return Job{}, false, err
	}
	// Recheck in a fresh statement snapshot after obtaining the group lock.
	var capacity bool
	if err := tx.QueryRow(ctx, `select (select count(*) from documentation_jobs j
where j.group_id=g.id and j.status in ('analyzing','generating','validating') and j.lease_expires_at > now()) < g.max_concurrent_jobs
from gitlab_groups g where g.id=$1`, groupID).Scan(&capacity); err != nil {
		return Job{}, false, err
	}
	if !capacity {
		return Job{}, false, tx.Commit(ctx)
	}
	const q = `
with candidate as (
  select j.id
  from documentation_jobs j
  where j.group_id=$1
    and j.attempt_count < j.max_attempts
    and (j.next_run_at is null or j.next_run_at <= now())
    and (j.status='queued' or (j.status in ('analyzing','generating','validating') and (j.lease_expires_at is null or j.lease_expires_at <= now())))
  order by j.created_at
  for update skip locked
  limit 1
)
update documentation_jobs j
set status='analyzing', lease_expires_at=now()+($2 * interval '1 second'), attempt_count=attempt_count+1, error=null, updated_at=now()
from candidate
where j.id = candidate.id
returning j.id, j.project_id, j.project_name, j.group_id, j.commit_sha, j.branch, j.idempotency_key, j.archive_path, j.status, coalesce(j.error,''), j.created_at, j.updated_at`
	var job Job
	err = tx.QueryRow(ctx, q, groupID, leaseFor.Seconds()).Scan(&job.ID, &job.ProjectID, &job.ProjectName, &job.GroupID, &job.CommitSHA, &job.Branch, &job.IdempotencyKey, &job.ArchivePath, &job.Status, &job.Error, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, tx.Commit(ctx)
	}
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (s *PostgresStore) UpdateStatus(ctx context.Context, id string, status Status, message string) error {
	result, err := s.db.Exec(ctx, `update documentation_jobs set status=$2, error=nullif($3,''),
lease_expires_at=case when $2 in ('completed','failed','cancelled') then null else lease_expires_at end,
updated_at=now() where id=$1`, id, status, message)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
