-- Admin analytics: accounts, their people and apps, and platform growth.

-- name: AdminAccounts :many
-- One row per account with its activation stage (counted independently).
select o.id, o.name, o.created_at,
  (select max(u.last_seen_at) from memberships m join users u on u.id = m.user_id where m.org_id = o.id)::timestamptz as last_active,
  exists (select 1 from projects p where p.org_id = o.id) as has_app,
  exists (select 1 from projects p where p.org_id = o.id and (
    exists (select 1 from daily_overview d where d.project_id = p.id)
    or exists (select 1 from issues i where i.project_id = p.id))) as connected,
  exists (select 1 from projects p join issues i on i.project_id = p.id where p.org_id = o.id) as caught,
  exists (select 1 from projects p join issues i on i.project_id = p.id join fix_suggestions s on s.issue_id = i.id
          where p.org_id = o.id) as prompted,
  exists (select 1 from projects p join issues i on i.project_id = p.id join fix_feedback f on f.issue_id = i.id
          where p.org_id = o.id and f.outcome = 'worked') as fixed,
  (select coalesce(sum(s.cost_usd), 0) from projects p join issues i on i.project_id = p.id
     join fix_suggestions s on s.issue_id = i.id where p.org_id = o.id)::float8 as ai_cost_usd
from orgs o order by o.created_at desc limit 500;

-- name: AdminAccountMembers :many
select m.org_id, u.id, u.email, u.role, u.created_at, u.last_seen_at,
  (select count(*) from user_activity_days a where a.user_id = u.id and a.day > current_date - 30)::bigint as active_days_30d
from memberships m join users u on u.id = m.user_id order by m.org_id, u.email;

-- name: SetUserRole :execrows
update users set role = @role where id = @id;

-- name: AdminAccountProjects :many
select p.id, p.org_id, p.name, p.target_agent, p.backend, p.created_at,
  (select coalesce(sum(d.visitors), 0) from daily_overview d where d.project_id = p.id and d.day > current_date - 7)::bigint as visitors_7d,
  (select coalesce(sum(d.visitors), 0) from daily_overview d where d.project_id = p.id and d.day > current_date - 30)::bigint as visitors_30d,
  (select coalesce(sum(d.signups), 0) from daily_overview d where d.project_id = p.id and d.day > current_date - 30)::bigint as signups_30d,
  (select count(*) from events e where e.project_id = p.id and e.received_at > now() - interval '24 hours')::bigint as errors_24h,
  (select count(*) from issues i where i.project_id = p.id and i.status not in ('fixed', 'ignored'))::bigint as open_issues,
  (select count(*) from issues i where i.project_id = p.id and i.status = 'fixed')::bigint as fixed_issues,
  (select max(i.last_seen_at) from issues i where i.project_id = p.id)::timestamptz as last_issue_at
from projects p order by p.org_id, p.id;

-- name: AdminGetAccount :one
select id, name, created_at from orgs where id = $1;

-- name: AdminPlatformTotals :one
-- "30d" is the last 30 days, "prev" the 30 days before, for trend arrows.
select
  (select count(*) from orgs)::bigint as accounts,
  (select count(*) from orgs where created_at > now() - interval '30 days')::bigint as accounts_30d,
  (select count(*) from orgs where created_at > now() - interval '60 days' and created_at <= now() - interval '30 days')::bigint as accounts_prev,
  (select count(*) from projects)::bigint as apps,
  (select count(*) from projects where created_at > now() - interval '30 days')::bigint as apps_30d,
  (select count(distinct user_id) from user_activity_days where day > current_date - 7)::bigint as active_users_7d,
  (select count(distinct user_id) from user_activity_days where day > current_date - 30)::bigint as active_users_30d,
  (select count(distinct project_id) from daily_overview where day > current_date - 7 and visitors > 0)::bigint as live_apps_7d,
  (select coalesce(sum(visitors), 0) from daily_overview where day > current_date - 30)::bigint as visitors_30d,
  (select coalesce(sum(visitors), 0) from daily_overview where day > current_date - 60 and day <= current_date - 30)::bigint as visitors_prev,
  (select count(*) from events where received_at > now() - interval '30 days')::bigint as errors_30d,
  (select count(*) from issues where first_seen_at > now() - interval '30 days')::bigint as issues_30d,
  (select count(*) from issues where first_seen_at > now() - interval '60 days' and first_seen_at <= now() - interval '30 days')::bigint as issues_prev,
  (select count(*) from issues where status not in ('fixed', 'ignored'))::bigint as open_issues,
  (select count(*) from issues where fixed_at > now() - interval '30 days')::bigint as fixed_30d,
  (select count(*) from fix_suggestions where created_at > now() - interval '30 days')::bigint as prompts_30d,
  (select count(*) from fix_feedback where outcome = 'worked' and created_at > now() - interval '30 days')::bigint as worked_30d,
  (select count(*) from fix_feedback where outcome = 'failed' and created_at > now() - interval '30 days')::bigint as failed_30d,
  (select coalesce(sum(cost_usd), 0) from fix_suggestions where created_at > now() - interval '30 days')::float8 as ai_cost_30d,
  (select coalesce(sum(cost_usd), 0) from fix_suggestions where created_at > now() - interval '60 days' and created_at <= now() - interval '30 days')::float8 as ai_cost_prev;

-- name: AdminDailySeries :many
-- The last 30 UTC days, one row per day, for the growth charts.
select d.day::date as day,
  (select count(*) from users u where u.created_at >= d.day and u.created_at < d.day + interval '1 day')::bigint as signups,
  (select count(*) from user_activity_days a where a.day = d.day::date)::bigint as active_users,
  (select coalesce(sum(o.visitors), 0) from daily_overview o where o.day = d.day::date)::bigint as visitors,
  (select count(*) from events e where e.received_at >= d.day and e.received_at < d.day + interval '1 day')::bigint as errors,
  (select count(*) from issues i where i.first_seen_at >= d.day and i.first_seen_at < d.day + interval '1 day')::bigint as new_issues,
  (select count(*) from fix_suggestions s where s.created_at >= d.day and s.created_at < d.day + interval '1 day')::bigint as prompts,
  (select coalesce(sum(s.cost_usd), 0) from fix_suggestions s where s.created_at >= d.day and s.created_at < d.day + interval '1 day')::float8 as ai_cost
from (select generate_series(current_date - 29, current_date, interval '1 day') as day) d
order by d.day;

-- name: AdminAccountDaily :many
-- The same 30 days for one account.
select d.day::date as day,
  (select count(*) from user_activity_days a join memberships m on m.user_id = a.user_id
   where m.org_id = @org_id and a.day = d.day::date)::bigint as active_users,
  (select coalesce(sum(o.visitors), 0) from daily_overview o join projects p on p.id = o.project_id
   where p.org_id = @org_id and o.day = d.day::date)::bigint as visitors,
  (select count(*) from events e join projects p on p.id = e.project_id
   where p.org_id = @org_id and e.received_at >= d.day and e.received_at < d.day + interval '1 day')::bigint as errors
from (select generate_series(current_date - 29, current_date, interval '1 day') as day) d
order by d.day;

-- name: AdminTopIssues :many
-- The open issues hurting the most people, across every account or one.
select i.id, i.project_id, p.name as app_name, o.id as org_id, o.name as org_name, i.title, i.kind, i.status,
  i.event_count, i.user_count, i.last_seen_at
from issues i join projects p on p.id = i.project_id join orgs o on o.id = p.org_id
where i.status not in ('fixed', 'ignored') and (sqlc.narg(org_id)::bigint is null or o.id = sqlc.narg(org_id))
order by i.impact_score desc, i.last_seen_at desc
limit 20;

-- name: AdminIssueKinds :many
select kind, count(*)::bigint as issues,
  count(*) filter (where status = 'fixed')::bigint as fixed,
  coalesce(sum(event_count), 0)::bigint as events
from issues group by kind order by issues desc;

-- name: AdminAppsByBuilder :many
select target_agent, count(*)::bigint as apps from projects group by target_agent order by apps desc;

-- name: AdminAIByModel :many
-- Cached and catalog prompts have no model: they're grouped by origin.
select coalesce(nullif(model, ''), origin)::text as model, count(*)::bigint as prompts,
  coalesce(sum(cost_usd), 0)::float8 as cost_usd,
  coalesce(avg(latency_ms), 0)::float8 as avg_latency_ms,
  coalesce(sum(tokens_in), 0)::bigint as tokens_in, coalesce(sum(tokens_out), 0)::bigint as tokens_out
from fix_suggestions where created_at > now() - interval '30 days'
group by 1 order by cost_usd desc, prompts desc;
