create extension if not exists pgcrypto;

create table gitlab_groups (
  id text primary key,
  name text not null,
  enabled boolean not null default true,
  allowed_projects text[] not null default '{}',
  max_concurrent_jobs integer not null default 1 check (max_concurrent_jobs > 0),
  documentation_policy jsonb not null default '{}',
  model_config jsonb not null default '{}',
  job_timeout interval not null default interval '30 minutes',
  archive_size_limit_bytes bigint not null default 104857600,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table projects (
  id text primary key,
  group_id text not null references gitlab_groups(id),
  path text not null,
  name text not null,
  enabled boolean not null default true,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table documentation_jobs (
  id text primary key default gen_random_uuid()::text,
  project_id text not null,
  project_name text not null default '',
  group_id text not null references gitlab_groups(id),
  commit_sha text not null,
  branch text not null,
  idempotency_key text not null unique,
  archive_path text not null default '',
  status text not null check (status in ('queued','analyzing','generating','validating','completed','failed','cancelled')),
  attempt_count integer not null default 0,
  max_attempts integer not null default 3,
  next_run_at timestamptz,
  lease_expires_at timestamptz,
  error text,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create index documentation_jobs_status_idx on documentation_jobs(status, next_run_at, created_at);
create index documentation_jobs_group_status_idx on documentation_jobs(group_id, status, lease_expires_at);

create table job_attempts (
  id bigserial primary key,
  job_id text not null references documentation_jobs(id) on delete cascade,
  attempt_number integer not null,
  started_at timestamptz not null default now(),
  finished_at timestamptz,
  status text not null,
  error text
);

create table ingestion_requests (
  id bigserial primary key,
  job_id text references documentation_jobs(id) on delete set null,
  project_id text not null,
  group_id text not null,
  commit_sha text not null,
  branch text not null,
  oidc_subject text not null,
  archive_bytes bigint not null,
  remote_addr inet,
  created_at timestamptz not null default now()
);

create table analysis_snapshots (
  id bigserial primary key,
  job_id text not null references documentation_jobs(id) on delete cascade,
  project_id text not null,
  commit_sha text not null,
  snapshot jsonb not null,
  created_at timestamptz not null default now()
);

create table evidence_items (
  id text primary key,
  job_id text not null references documentation_jobs(id) on delete cascade,
  project_id text not null,
  commit_sha text not null,
  category text not null,
  type text not null,
  value text not null,
  source_file text not null,
  line_start integer,
  line_end integer,
  verification text not null,
  confidence text not null,
  related_components text[] not null default '{}',
  created_at timestamptz not null default now()
);

create table generated_documents (
  id bigserial primary key,
  job_id text not null references documentation_jobs(id) on delete cascade,
  document_type text not null,
  content_markdown text not null,
  validation_status text not null,
  created_at timestamptz not null default now()
);

create table documentation_artifacts (
  id bigserial primary key,
  job_id text not null references documentation_jobs(id) on delete cascade,
  artifact_path text not null,
  content_type text not null,
  sha256 text not null,
  bytes bigint not null,
  created_at timestamptz not null default now()
);

create table publication_records (
  id bigserial primary key,
  job_id text not null references documentation_jobs(id) on delete cascade,
  target text not null,
  status text not null,
  details jsonb not null default '{}',
  created_at timestamptz not null default now()
);
