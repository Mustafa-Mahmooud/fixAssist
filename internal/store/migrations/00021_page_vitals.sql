-- +goose Up

-- Page speed (Core Web Vitals). The SDK sends each measurement as an
-- analytics event of type 'vital' (name: LCP, INP, CLS, FCP or TTFB;
-- props.v the value, props.rel the release, plus attribution), so it shares
-- that table's consent rules, batching and 30-day retention.
alter table analytics_events drop constraint analytics_events_type_check;
alter table analytics_events add constraint analytics_events_type_check
  check (type in ('session_start', 'click', 'custom', 'identify', 'signup', 'login', 'vital'));

-- Daily histograms, kept forever: how many measurements fell in each bucket
-- (analytics.VitalEdges). Percentiles are computed from these, so any range
-- of days, devices or releases can be combined; a stored p75 couldn't be.
create table daily_vitals (
  project_id bigint not null references projects(id) on delete cascade,
  day date not null,
  route text not null,
  device text not null,
  release_id text not null default '',
  metric text not null check (metric in ('LCP', 'INP', 'CLS', 'FCP', 'TTFB')),
  bucket smallint not null,
  n integer not null,
  primary key (project_id, day, route, device, release_id, metric, bucket)
);

-- +goose Down
drop table daily_vitals;
delete from analytics_events where type = 'vital';
alter table analytics_events drop constraint analytics_events_type_check;
alter table analytics_events add constraint analytics_events_type_check
  check (type in ('session_start', 'click', 'custom', 'identify', 'signup', 'login'));
