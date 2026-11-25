-- +goose Up

-- Agent loop: the founder's coding agent (Cursor, Claude Code, ...) talks to
-- Fix Assistant over MCP with a per-project token, fixes an issue, reports
-- the fix, and checks whether it held, attempt after attempt.

-- Tokens are shown once; only their SHA-256 is stored.
create table agent_tokens (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  name text not null,
  token_hash text not null unique,
  prefix text not null,                 -- first characters, to tell tokens apart
  created_by bigint references users(id) on delete set null,
  created_at timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at timestamptz
);
create index agent_tokens_project on agent_tokens (project_id);

-- One row per fix applied to an issue (by an agent or the founder), with
-- what was changed, so the next attempt's prompt knows what didn't work.
create table fix_attempts (
  id bigint generated always as identity primary key,
  issue_id bigint not null references issues(id) on delete cascade,
  suggestion_id bigint references fix_suggestions(id) on delete set null,
  attempt integer not null,
  source text not null check (source in ('agent', 'founder')),
  agent_token_id bigint references agent_tokens(id) on delete set null,
  release_id text,                      -- as reported, optional
  summary text not null default '',
  files text[] not null default '{}',
  reported_at timestamptz not null default now(),
  reproduced_at timestamptz,            -- the agent repeated the failing action
  outcome text check (outcome in ('worked', 'failed')),
  outcome_at timestamptz
);
create index fix_attempts_issue on fix_attempts (issue_id, reported_at desc);

-- +goose Down
drop table fix_attempts;
drop table agent_tokens;
