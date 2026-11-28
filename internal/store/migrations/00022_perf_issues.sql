-- +goose Up

-- Page-speed regressions as issues (kind 'perf'): "Loading on /pricing got
-- slower". They have no error events: internal/perf detects them from
-- daily_vitals, explains them from perf_regressions and verifies them by
-- speed, not by events stopping.
alter table issues drop constraint issues_kind_check;
alter table issues add constraint issues_kind_check check (kind in ('js', 'net', 'message', 'server', 'perf'));

-- What a perf issue is about, refreshed on each detection run.
create table perf_regressions (
  issue_id bigint primary key references issues(id) on delete cascade,
  project_id bigint not null references projects(id) on delete cascade,
  route text not null,
  metric text not null check (metric in ('LCP', 'INP', 'CLS')),
  device text not null,                 -- mobile or desktop
  baseline_p75 float8 not null,
  baseline_n bigint not null,
  current_p75 float8 not null,
  current_n bigint not null,
  current_release text not null default '', -- '' when compared by date (no new release)
  since timestamptz not null,           -- when the slower period began
  element text not null default '',     -- what slow visits most often point at
  source text not null default '',
  breakdown jsonb not null default '{}', -- where the time went (averages, ms)
  detected_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

-- A regression must show on two runs in a row before it becomes an issue.
create table perf_candidates (
  project_id bigint not null references projects(id) on delete cascade,
  route text not null,
  metric text not null,
  first_seen_at timestamptz not null default now(),
  primary key (project_id, route, metric)
);

-- +goose Down
drop table perf_candidates;
drop table perf_regressions;
delete from issues where kind = 'perf';
alter table issues drop constraint issues_kind_check;
alter table issues add constraint issues_kind_check check (kind in ('js', 'net', 'message', 'server'));
