create table notion_targets (
  group_id text primary key,
  parent_page_id text not null,
  enabled boolean not null default true,
  activated_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

alter table publication_records
  add column attempt_count integer not null default 0,
  add column next_run_at timestamptz not null default now(),
  add column updated_at timestamptz not null default now();
create unique index publication_records_job_target_key on publication_records(job_id, target);
create index publication_records_pending_idx on publication_records(status, next_run_at);

create table notion_pages (
  parent_page_id text not null,
  project_id text not null,
  job_id text not null default '',
  kind text not null,
  page_id text not null,
  primary key (parent_page_id, project_id, job_id, kind)
);

-- Shared cooldown and a session advisory lock coordinate all publishing replicas.
create table notion_publication_control (
  id boolean primary key default true check (id),
  next_request_at timestamptz not null default now()
);
insert into notion_publication_control(id) values(true);
