-- name: InsertEvent :one
insert into events (project_id, received_at, kind, fingerprint, release_id, route, user_hash, payload)
values ($1, $2, $3, $4, $5, $6, $7, $8)
returning id;

-- name: BumpUsage :exec
insert into project_usage_monthly (project_id, month, events)
values ($1, $2, $3)
on conflict (project_id, month) do update set events = project_usage_monthly.events + excluded.events;

-- name: InsertPageview :exec
insert into pageviews (project_id, ts, route, session_id, release_id, load_ms, visitor_hash, country, device)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: InsertAnalyticsEvent :exec
insert into analytics_events (project_id, ts, type, session_id, visitor_hash, account_hash, route, name, props,
  referrer_host, utm_source, utm_medium, utm_campaign, country, device)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: GetEventForGrouping :one
select id, received_at, project_id, kind, fingerprint, grouped_at
from events
where id = $1 and received_at = $2;

-- name: MarkEventGrouped :execrows
update events set grouped_at = now()
where id = $1 and received_at = $2 and grouped_at is null;
