-- +goose Up

-- Auto-fix: when an issue is explained with high severity, Fix Assistant
-- asks the founder (or a rule) to approve a fix, and the founder's local
-- daemon (cmd/fixassist) runs their coding agent on it in a separate git
-- worktree, opens a pull request and, when allowed, turns on auto-merge.

alter table projects add column autofix_enabled boolean not null default false;
-- 'high' approves every high-severity issue without asking. Such runs open
-- a pull request only: merging still needs a person to approve the diff,
-- because the evidence in the prompt comes from the app's own visitors.
alter table projects add column autofix_rule text not null default 'off'
  check (autofix_rule in ('off', 'high'));

-- A daemon is online while it polls; one row per agent token.
create table agent_daemons (
  token_id bigint primary key references agent_tokens(id) on delete cascade,
  project_id bigint not null references projects(id) on delete cascade,
  hostname text not null default '',
  version text not null default '',
  last_seen_at timestamptz not null default now()
);
create index agent_daemons_project on agent_daemons (project_id, last_seen_at desc);

create table autofix_runs (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  issue_id bigint not null references issues(id) on delete cascade,
  suggestion_id bigint references fix_suggestions(id) on delete set null,
  state text not null check (state in (
    'pending_approval',        -- waiting for the founder
    'approved',                -- queued for the daemon
    'claimed',                 -- the daemon is working on it (lease_until)
    'pr_opened',               -- pull request open (auto-merge on if auto_merge)
    'awaiting_merge_approval', -- rule-approved: a person must approve the diff
    'merge_approved',          -- approved: the daemon turns on auto-merge
    'merged',                  -- merged; the fix is being verified
    'failed', 'rejected', 'expired',
    'emailed'                  -- the daemon was offline: the prompt was emailed
  )),
  approved_by text check (approved_by in ('user', 'rule')),
  approved_user_id bigint references users(id) on delete set null,
  approved_at timestamptz,
  auto_merge boolean not null default false,
  token_id bigint references agent_tokens(id) on delete set null,
  claimed_at timestamptz,
  lease_until timestamptz,
  branch text not null default '',
  pr_url text not null default '',
  summary text not null default '',
  files text[] not null default '{}',
  error text not null default '',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
-- At most one run in progress per issue.
create unique index autofix_runs_one_open on autofix_runs (issue_id)
  where state in ('pending_approval', 'approved', 'claimed', 'pr_opened', 'awaiting_merge_approval', 'merge_approved');
create index autofix_runs_project on autofix_runs (project_id, created_at desc);
create index autofix_runs_state on autofix_runs (state, updated_at) where state in ('pending_approval', 'approved', 'claimed');

-- Approval requests and offline fallbacks are stored like other alerts.
alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check
  check (kind in ('instant', 'digest', 'quota80', 'quota100', 'growth', 'autofix'));

-- +goose Down
delete from alert_log where kind = 'autofix';
alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check
  check (kind in ('instant', 'digest', 'quota80', 'quota100', 'growth'));
drop table autofix_runs;
drop table agent_daemons;
alter table projects drop column autofix_rule;
alter table projects drop column autofix_enabled;
