-- +goose Up

-- Page views gain the anonymous visitor, country and device class.
-- visitor_hash: cookieless daily hash (IP + user agent + project, rotating
-- salt) or, after consent, an HMAC of the SDK's first-party id.
alter table pageviews
  add column visitor_hash text,
  add column country text,
  add column device text;

-- Raw analytics events, daily partitions, 30 days like errors (FDR Phase 6).
create table analytics_events (
  id bigserial not null,
  project_id bigint not null,
  ts timestamptz not null default now(),
  type text not null check (type in ('session_start', 'click', 'custom', 'identify', 'signup', 'login')),
  session_id text,
  visitor_hash text,
  account_hash text,
  route text,
  name text,                 -- click label or custom event name
  props jsonb,
  referrer_host text,
  utm_source text,
  utm_medium text,
  utm_campaign text,
  country text,
  device text,
  primary key (ts, id)
) partition by range (ts);
create index analytics_events_project_ts on analytics_events (project_id, ts);

-- Rollups, kept forever; screens read only these.
create table daily_overview (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  visitors integer not null default 0,
  sessions integer not null default 0,
  pageviews integer not null default 0,
  signups integer not null default 0,
  logins integer not null default 0,
  primary key (project_id, day)
);

create table daily_page_stats (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  route text not null,
  device text not null,
  views integer not null default 0,
  visitors integer not null default 0,
  sessions integer not null default 0,
  exits integer not null default 0,    -- sessions whose last page view was this route
  load_ms_sum bigint not null default 0,
  load_ms_count integer not null default 0,
  primary key (project_id, day, route, device)
);

create table daily_source_stats (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  source text not null,     -- utm_source, else referrer host, else 'direct'
  medium text not null default '',
  campaign text not null default '',
  sessions integer not null default 0,
  visitors integer not null default 0,
  signups integer not null default 0,
  primary key (project_id, day, source, medium, campaign)
);

create table daily_click_stats (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  route text not null,
  label text not null,
  clicks integer not null default 0,
  visitors integer not null default 0,
  primary key (project_id, day, route, label)
);

create table daily_event_stats (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  name text not null,
  events integer not null default 0,
  visitors integer not null default 0,
  primary key (project_id, day, name)
);

create table daily_dimension_stats (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  dimension text not null check (dimension in ('device', 'country')),
  value text not null,
  sessions integer not null default 0,
  visitors integer not null default 0,
  primary key (project_id, day, dimension, value)
);

-- Conversion goals: signups are detected automatically; founders add more.
create table goals (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  name text not null,
  kind text not null check (kind in ('signup', 'page', 'click', 'event')),
  match text not null default '',   -- route, click label or event name
  is_lead boolean not null default false,
  created_at timestamptz not null default now()
);

create table daily_goal_stats (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  goal_id bigint not null references goals(id) on delete cascade,
  conversions integer not null default 0,   -- sessions that reached the goal
  visitors integer not null default 0,
  primary key (project_id, day, goal_id)
);

-- Funnels: ordered steps [{"kind":"page|click|event|goal","match":"..."}].
create table funnels (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  name text not null,
  steps jsonb not null,
  created_at timestamptz not null default now()
);

create table daily_funnel_stats (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  funnel_id bigint not null references funnels(id) on delete cascade,
  step integer not null,
  device text not null,
  sessions integer not null default 0,   -- sessions that reached this step in order
  primary key (project_id, day, funnel_id, step, device)
);

-- Retention by signup week for identified accounts (fix.identify).
create table weekly_retention (
  project_id bigint not null references projects(id) on delete cascade,
  cohort_week date not null,
  week_offset integer not null,
  accounts integer not null default 0,
  primary key (project_id, cohort_week, week_offset)
);

-- Weekly growth insights and their before/after check.
create table growth_insights (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  week date not null,
  detector text not null,
  title text not null,
  evidence text not null,
  hypothesis text not null,
  fix_prompt text not null,
  how_to_check text not null,
  metric jsonb not null,            -- what to re-measure after the next release
  baseline numeric not null,
  baseline_days integer not null default 7,
  release_id text,                  -- latest release when the insight was made
  after_value numeric,
  after_release_id text,
  outcome text check (outcome in ('worked', 'no_change', 'worse')),
  outcome_source text check (outcome_source in ('explicit', 'implicit')),
  created_at timestamptz not null default now(),
  unique (project_id, week, detector, title)
);

alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check
  check (kind in ('instant', 'digest', 'quota80', 'quota100', 'growth'));
create unique index alert_log_growth_once on alert_log (project_id, day) where kind = 'growth';

-- +goose Down
drop index alert_log_growth_once;
alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check check (kind in ('instant', 'digest', 'quota80', 'quota100'));
drop table growth_insights;
drop table weekly_retention;
drop table daily_funnel_stats;
drop table funnels;
drop table daily_goal_stats;
drop table goals;
drop table daily_dimension_stats;
drop table daily_event_stats;
drop table daily_click_stats;
drop table daily_source_stats;
drop table daily_page_stats;
drop table daily_overview;
drop table analytics_events;
alter table pageviews drop column device, drop column country, drop column visitor_hash;
