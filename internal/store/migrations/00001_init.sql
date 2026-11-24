-- +goose Up

create table orgs (
  id bigint generated always as identity primary key,
  name text not null,
  created_at timestamptz not null default now()
);

create table projects (
  id bigint generated always as identity primary key,
  org_id bigint not null references orgs(id),
  name text not null,
  target_agent text not null default 'other'
    check (target_agent in ('lovable', 'bolt', 'cursor', 'replit', 'v0', 'other')),
  backend text not null default 'unknown'
    check (backend in ('supabase', 'firebase', 'unknown')),
  stack_tags text[] not null default '{}',
  allowed_domains text[] not null default '{}',
  quota_monthly bigint not null default 500000,
  -- Key for user_hash = HMAC-SHA256(hmac_secret, user id). Never leaves the server.
  hmac_secret bytea not null,
  created_at timestamptz not null default now()
);

create table project_keys (
  public_key text primary key,
  project_id bigint not null references projects(id),
  created_at timestamptz not null default now(),
  revoked_at timestamptz
);
create index project_keys_project_id on project_keys (project_id);

create table project_usage_monthly (
  project_id bigint not null references projects(id),
  month date not null,
  events bigint not null default 0,
  primary key (project_id, month)
);

-- Raw events, daily partitions (created and dropped by the partitions job).
-- Postgres 16 does not allow identity columns on partitioned tables, so bigserial.
create table events (
  id bigserial not null,
  project_id bigint not null,
  received_at timestamptz not null default now(),
  kind text not null check (kind in ('js', 'net', 'message')),
  fingerprint text not null,
  issue_id bigint,
  release_id text,
  route text,
  user_hash text,
  -- null when the per-fingerprint hourly sample cap was hit
  payload jsonb,
  grouped_at timestamptz,
  primary key (received_at, id)
) partition by range (received_at);
create index events_project_received on events (project_id, received_at);
create index events_ungrouped on events (received_at) where grouped_at is null;

create table pageviews (
  id bigserial not null,
  project_id bigint not null,
  ts timestamptz not null default now(),
  route text not null,
  session_id text,
  release_id text,
  load_ms integer,
  primary key (ts, id)
) partition by range (ts);
create index pageviews_project_ts on pageviews (project_id, ts);

-- +goose Down
drop table pageviews;
drop table events;
drop table project_usage_monthly;
drop table project_keys;
drop table projects;
drop table orgs;
