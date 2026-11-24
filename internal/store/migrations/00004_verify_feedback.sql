-- +goose Up

-- Verification state (FDR "Fix feedback and verification").
--   watch_since: events from releases first seen at/after this time count as
--     "the fix didn't work" (stale tabs on older releases don't).
--   verify_release_id: the new release being watched while verifying.
--   probably_fixed_at: low traffic, 7 days on the new release without enough
--     sessions; the dashboard asks the founder instead of deciding.
alter table issues
  add column watch_since timestamptz,
  add column verify_release_id text,
  add column probably_fixed_at timestamptz;

-- Did it work? Explicit answers always win over implicit verification.
create table fix_feedback (
  id bigint generated always as identity primary key,
  suggestion_id bigint references fix_suggestions(id) on delete cascade,
  issue_id bigint not null references issues(id) on delete cascade,
  outcome text not null check (outcome in ('worked', 'failed', 'partial')),
  source text not null check (source in ('explicit', 'implicit')),
  release_id text,
  user_id bigint references users(id) on delete set null,
  created_at timestamptz not null default now()
);
create index fix_feedback_issue on fix_feedback (issue_id, created_at desc);

-- Quota warnings (80% and 100% of the monthly event quota), once per month.
alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check
  check (kind in ('instant', 'digest', 'quota80', 'quota100'));
create unique index alert_log_quota_once on alert_log (project_id, kind, day) where kind in ('quota80', 'quota100');

-- +goose Down
drop index alert_log_quota_once;
alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check check (kind in ('instant', 'digest'));
drop table fix_feedback;
alter table issues drop column probably_fixed_at, drop column verify_release_id, drop column watch_since;
