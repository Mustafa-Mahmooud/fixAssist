-- +goose Up

-- What we told the founder for an issue (FDR fix_suggestions). In agent mode
-- the explanation comes from the catalog or the issue data, and the fix
-- prompt asks the founder's own AI agent to do the code-level work.
create table fix_suggestions (
  id bigint generated always as identity primary key,
  issue_id bigint not null references issues(id) on delete cascade,
  -- catalog: known error; agent: diagnose-and-fix prompt; model: our LLM (later)
  origin text not null check (origin in ('catalog', 'agent', 'model', 'cache')),
  catalog_key text,
  prompt_version text not null,
  target_agent text not null,
  severity text not null check (severity in ('low', 'medium', 'high')),
  title text not null,
  what_broke text not null,
  who_is_affected text not null,
  why text not null default '',
  fix_prompt text not null,   -- base prompt; agent adapters are applied when shown
  how_to_check text not null,
  confidence numeric not null default 0,
  attempt integer not null default 1, -- 2+ after the previous fix didn't work
  model text,
  tokens_in integer not null default 0,
  tokens_out integer not null default 0,
  cost_usd numeric not null default 0,
  latency_ms integer not null default 0,
  created_at timestamptz not null default now()
);
create index fix_suggestions_issue on fix_suggestions (issue_id, created_at desc);

-- Groundwork for the shared cache (FDR Phase 7). No customer data: only the
-- normalized signature and the parameterized template. Embeddings are stored
-- as real[] until production runs pgvector (then vector(N) + HNSW).
create table knowledge_entries (
  id bigint generated always as identity primary key,
  signature text not null unique,
  error_type text not null,
  framework text not null default '',
  catalog_key text,
  embedding real[],
  explanation_template text not null,
  prompts_by_platform jsonb not null default '{}',
  worked integer not null default 0,
  failed integer not null default 0,
  confidence numeric not null default 0.5,
  created_at timestamptz not null default now()
);

alter table issues add column explained_at timestamptz;

-- Daily digest at 09:00 local time.
alter table projects add column timezone text not null default 'UTC';

-- Every alert, emailed or not; the dashboard shows these, so nothing is lost
-- while email is off.
create table alert_log (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  issue_id bigint references issues(id) on delete cascade,
  kind text not null check (kind in ('instant', 'digest')),
  day date not null,
  subject text not null,
  body text not null,
  emailed_to text[] not null default '{}',
  sent_at timestamptz not null default now()
);
-- FDR idempotency: one instant alert per issue per day, one digest per project per day.
create unique index alert_log_instant_once on alert_log (issue_id, day) where kind = 'instant';
create unique index alert_log_digest_once on alert_log (project_id, day) where kind = 'digest';
create index alert_log_project on alert_log (project_id, sent_at desc);

-- +goose Down
drop table alert_log;
alter table projects drop column timezone;
alter table issues drop column explained_at;
drop table knowledge_entries;
drop table fix_suggestions;
