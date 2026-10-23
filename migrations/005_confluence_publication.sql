create table confluence_targets (
  group_id text primary key,
  destination_key text not null,
  space_id text not null default '',
  space_key text not null default '',
  parent_page_id text not null,
  enabled boolean not null default false,
  activated_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table confluence_pages (
  destination_key text not null,
  project_id text not null,
  job_id text not null default '',
  kind text not null,
  page_id text not null,
  primary key (destination_key, project_id, job_id, kind)
);

create table confluence_publication_control (
  id boolean primary key default true check (id),
  next_request_at timestamptz not null default now()
);
insert into confluence_publication_control(id) values(true);
