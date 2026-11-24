-- +goose Up

-- Accounts --------------------------------------------------------------

create table users (
  id bigint generated always as identity primary key,
  email text not null,
  github_id bigint unique,
  created_at timestamptz not null default now()
);
create unique index users_email_lower on users (lower(email));

create table memberships (
  user_id bigint not null references users(id) on delete cascade,
  org_id bigint not null references orgs(id) on delete cascade,
  role text not null default 'owner' check (role in ('owner', 'member')),
  created_at timestamptz not null default now(),
  primary key (user_id, org_id)
);

-- Single-use magic-link tokens; only the SHA-256 is stored.
create table login_tokens (
  token_hash bytea primary key,
  email text not null,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at timestamptz
);
create index login_tokens_email_created on login_tokens (lower(email), created_at);

-- Browser sessions; only the SHA-256 of the cookie value is stored.
create table web_sessions (
  id_hash bytea primary key,
  user_id bigint not null references users(id) on delete cascade,
  csrf_token text not null,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null
);
create index web_sessions_user on web_sessions (user_id);

-- Key pages (checkout, signup, login) weigh 3x in the impact score.
-- Guessed from route names until the founder edits them in settings.
alter table projects add column key_pages text[] not null default '{}';

-- Releases and issues -----------------------------------------------------

create table releases (
  project_id bigint not null references projects(id) on delete cascade,
  release_id text not null,
  first_seen_at timestamptz not null,
  last_seen_at timestamptz not null,
  event_count bigint not null default 0,
  primary key (project_id, release_id)
);

create table issues (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  fingerprint text not null,
  kind text not null check (kind in ('js', 'net', 'message')),
  signature text not null,
  title text not null,
  route text,
  status text not null default 'new'
    check (status in ('new', 'explained', 'verifying', 'fixed', 'open', 'ignored', 'analysis_failed')),
  first_seen_at timestamptz not null,
  last_seen_at timestamptz not null,
  event_count bigint not null default 0,
  user_count bigint not null default 0,
  first_release_id text,
  last_release_id text,
  -- set when the issue is marked fixed (Phase 5 verification or by hand)
  fixed_at timestamptz,
  fixed_release_id text,
  regressed_at timestamptz,
  regression_count integer not null default 0,
  impact_score numeric not null default 0,
  unique (project_id, fingerprint)
);
create index issues_project_impact on issues (project_id, impact_score desc, last_seen_at desc);

create table issue_users (
  issue_id bigint not null references issues(id) on delete cascade,
  user_hash text not null,
  first_seen_at timestamptz not null,
  last_seen_at timestamptz not null,
  primary key (issue_id, user_hash)
);
create index issue_users_recent on issue_users (issue_id, last_seen_at);

create table issue_hourly (
  issue_id bigint not null references issues(id) on delete cascade,
  hour timestamptz not null,
  events integer not null default 0,
  users integer not null default 0,
  primary key (issue_id, hour)
);

-- A few full events per issue, rolling (max 20).
create table issue_samples (
  id bigint generated always as identity primary key,
  issue_id bigint not null references issues(id) on delete cascade,
  event_id bigint not null,
  received_at timestamptz not null,
  release_id text,
  payload jsonb not null
);
create index issue_samples_issue on issue_samples (issue_id, received_at desc);

-- +goose Down
drop table issue_samples;
drop table issue_hourly;
drop table issue_users;
drop table issues;
drop table releases;
alter table projects drop column key_pages;
drop table web_sessions;
drop table login_tokens;
drop table memberships;
drop table users;
