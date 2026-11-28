-- name: RecordRejectedOrigin :exec
-- New hosts only while the project has fewer than 20 seen this week, so
-- forged Origin headers cannot fill the table (or silence it for good).
insert into project_rejected_origins (project_id, host)
select @project_id, @host::text
where (select count(*) from project_rejected_origins where project_id = @project_id
         and last_seen_at > now() - interval '7 days') < 20
   or exists (select 1 from project_rejected_origins where project_id = @project_id and host = @host::text)
on conflict (project_id, host) do update
  set last_seen_at = now(), hits = project_rejected_origins.hits + 1;

-- name: ListRejectedOrigins :many
select host, hits, last_seen_at from project_rejected_origins
where project_id = $1 and last_seen_at > now() - interval '7 days'
order by last_seen_at desc limit 5;

-- name: DeleteRejectedOrigin :exec
delete from project_rejected_origins where project_id = $1 and host = $2;

-- name: AddAllowedDomain :exec
update projects set allowed_domains = array_append(allowed_domains, @domain::text)
where id = @id and not (@domain::text = any(allowed_domains));

-- name: SetDetectedBackend :execrows
-- Only fills in a backend the founder did not choose.
update projects set backend = @backend where id = @id and backend = 'unknown';

-- name: FirstIssueID :one
-- Errors only: a slowdown isn't the "first issue caught" moment of setup.
select coalesce((select i.id from issues i where i.project_id = $1 and i.kind <> 'perf'
  order by i.impact_score desc, i.last_seen_at desc limit 1), 0)::bigint;

-- name: IssueRank :one
-- 1 for the project first issue, 2 for the second, ...
select count(*)::bigint from issues where project_id = @project_id and id <= @id;

-- name: ProjectsForSetupNudge :many
-- Apps added 1 to 14 days ago, with the setup list still shown, that have not
-- had both reminders yet. connected/has_issue tell which one applies.
select p.id, p.name, p.timezone, p.target_agent, p.allowed_domains, p.created_at,
  (select k.public_key from project_keys k where k.project_id = p.id and k.revoked_at is null order by k.created_at desc limit 1)::text as public_key,
  (exists (select 1 from pageviews v where v.project_id = p.id)
   or exists (select 1 from events e where e.project_id = p.id))::boolean as connected,
  exists (select 1 from issues i where i.project_id = p.id)::boolean as has_issue,
  exists (select 1 from alert_log a where a.project_id = p.id and a.kind = 'setup_install')::boolean as install_sent,
  exists (select 1 from alert_log a where a.project_id = p.id and a.kind = 'setup_test')::boolean as test_sent
from projects p
where p.onboarding_dismissed_at is null
  and p.created_at < sqlc.arg(as_of)::timestamptz - interval '1 day' and p.created_at > sqlc.arg(as_of)::timestamptz - interval '14 days'
  and not (exists (select 1 from alert_log a where a.project_id = p.id and a.kind = 'setup_install')
       and exists (select 1 from alert_log a where a.project_id = p.id and a.kind = 'setup_test'))
order by p.id;


-- name: AdminSetupTimes :many
-- When each app added in the last 30 days reached each setup step (null if
-- not yet), and when its setup reminder went out.
select p.created_at,
  least((select min(v.ts) from pageviews v where v.project_id = p.id),
        (select min(e.received_at) from events e where e.project_id = p.id))::timestamptz as connected_at,
  (select min(i.first_seen_at) from issues i where i.project_id = p.id)::timestamptz as issue_at,
  (select min(f.created_at) from fix_suggestions f join issues i on i.id = f.issue_id where i.project_id = p.id)::timestamptz as explained_at,
  (select min(b.created_at) from fix_feedback b join issues i on i.id = b.issue_id
     where i.project_id = p.id and b.outcome = 'worked')::timestamptz as fixed_at,
  (select min(a.sent_at) from alert_log a where a.project_id = p.id and a.kind = 'setup_install')::timestamptz as install_nudge_at,
  (select min(a.sent_at) from alert_log a where a.project_id = p.id and a.kind = 'setup_test')::timestamptz as test_nudge_at
from projects p where p.created_at > now() - interval '30 days'
order by p.created_at;
