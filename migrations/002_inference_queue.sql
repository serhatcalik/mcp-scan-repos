create table inference_pools (
  id text primary key,
  max_running integer not null check (max_running > 0),
  max_waiting integer not null check (max_waiting > 0)
);

create table inference_requests (
  id text primary key,
  pool_id text not null references inference_pools(id) on delete cascade,
  sequence bigserial not null,
  status text not null check (status in ('waiting','running')),
  lease_expires_at timestamptz not null
);

create index inference_requests_pool_idx
  on inference_requests(pool_id, status, sequence);
