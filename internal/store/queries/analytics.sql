-- Dashboard reads (rollups only). $2..$3 is an inclusive day range.

-- name: OverviewTotals :one
select coalesce(sum(visitors), 0)::bigint as visitors, coalesce(sum(sessions), 0)::bigint as sessions,
       coalesce(sum(pageviews), 0)::bigint as pageviews, coalesce(sum(signups), 0)::bigint as signups,
       coalesce(sum(logins), 0)::bigint as logins
from daily_overview where project_id = $1 and day between $2 and $3;

-- name: OverviewDaily :many
select day, visitors, sessions, pageviews, signups
from daily_overview where project_id = $1 and day between $2 and $3 order by day;

-- name: TopSources :many
select source, coalesce(sum(sessions), 0)::bigint as sessions, coalesce(sum(visitors), 0)::bigint as visitors,
       coalesce(sum(signups), 0)::bigint as signups
from daily_source_stats where project_id = $1 and day between $2 and $3
group by source order by sessions desc limit 10;

-- name: TopPages :many
select s.route,
       coalesce(sum(s.views), 0)::bigint as views, coalesce(sum(s.visitors), 0)::bigint as visitors,
       coalesce(sum(s.sessions), 0)::bigint as sessions, coalesce(sum(s.exits), 0)::bigint as exits,
       coalesce(sum(s.load_ms_sum) / nullif(sum(s.load_ms_count), 0), 0)::bigint as avg_load_ms,
       coalesce(sum(s.sessions) filter (where s.device = 'mobile'), 0)::bigint as mobile_sessions,
       coalesce(sum(s.exits) filter (where s.device = 'mobile'), 0)::bigint as mobile_exits,
       (select count(*) from issues i where i.project_id = s.project_id and i.route = s.route
          and i.status not in ('fixed', 'ignored'))::bigint as open_issues
from daily_page_stats s where s.project_id = $1 and s.day between $2 and $3
group by s.project_id, s.route order by views desc limit 30;

-- name: TopClicks :many
select route, label, coalesce(sum(clicks), 0)::bigint as clicks, coalesce(sum(visitors), 0)::bigint as visitors
from daily_click_stats where project_id = $1 and day between $2 and $3
group by route, label order by clicks desc limit 15;

-- name: TopEvents :many
select name, coalesce(sum(events), 0)::bigint as events, coalesce(sum(visitors), 0)::bigint as visitors
from daily_event_stats where project_id = $1 and day between $2 and $3
group by name order by events desc limit 15;

-- name: TopDimension :many
select value, coalesce(sum(sessions), 0)::bigint as sessions
from daily_dimension_stats where project_id = $1 and dimension = $2 and day between $3 and $4
group by value order by sessions desc limit 10;

-- Goals and funnels ---------------------------------------------------------

-- name: ListGoals :many
select id, name, kind, match, is_lead from goals where project_id = $1 order by id;

-- name: GoalStats :many
select goal_id, coalesce(sum(conversions), 0)::bigint as conversions, coalesce(sum(visitors), 0)::bigint as visitors
from daily_goal_stats where project_id = $1 and day between $2 and $3 group by goal_id;

-- name: InsertGoal :one
insert into goals (project_id, name, kind, match, is_lead) values ($1, $2, $3, $4, $5) returning id;

-- name: DeleteGoal :execrows
delete from goals where id = $1 and project_id = $2 and kind <> 'signup';

-- name: ListFunnels :many
select id, name, steps from funnels where project_id = $1 order by id;

-- name: FunnelStats :many
select funnel_id, step, device, coalesce(sum(sessions), 0)::bigint as sessions
from daily_funnel_stats where project_id = $1 and day between $2 and $3
group by funnel_id, step, device;

-- name: InsertFunnel :one
insert into funnels (project_id, name, steps) values ($1, $2, $3) returning id;

-- name: DeleteFunnel :execrows
delete from funnels where id = $1 and project_id = $2;

-- name: OpenIssuesForOverlay :many
-- Open issues per page, plus auth issues (for sign-up steps), for funnel overlays.
select id, title, route, user_count, signature
from issues where project_id = $1 and status not in ('fixed', 'ignored')
order by impact_score desc limit 200;

-- Growth insights -----------------------------------------------------------

-- name: ListInsights :many
select id, week, detector, title, evidence, hypothesis, fix_prompt, how_to_check, metric,
       baseline::float8 as baseline, (after_value is not null)::boolean as has_after,
       coalesce(after_value, 0)::float8 as after_value, outcome, outcome_source, created_at
from growth_insights where project_id = @project_id order by week desc, id
limit @lim offset @off;

-- name: CountInsights :one
select count(*) from growth_insights where project_id = $1;

-- name: SetInsightOutcome :execrows
update growth_insights set outcome = $3, outcome_source = 'explicit' where id = $1 and project_id = $2;

-- name: InsightsForWeek :many
select title, evidence from growth_insights where project_id = $1 and week = $2 order by id;
