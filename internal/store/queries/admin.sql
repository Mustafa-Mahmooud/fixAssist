-- name: AdminIngestByProject :many
select p.id, p.name, o.name as org_name,
  (select count(*) from events e where e.project_id = p.id and e.received_at > now() - interval '1 hour')::bigint as events_1h,
  (select count(*) from events e where e.project_id = p.id and e.received_at > now() - interval '24 hours')::bigint as events_24h,
  (select count(*) from issues i where i.project_id = p.id)::bigint as issues,
  (select count(*) from fix_suggestions s join issues i on i.id = s.issue_id where i.project_id = p.id)::bigint as suggestions,
  (select coalesce(sum(s.cost_usd), 0) from fix_suggestions s join issues i on i.id = s.issue_id where i.project_id = p.id)::float8 as ai_cost_usd,
  (select count(*) from fix_feedback f join issues i on i.id = f.issue_id where i.project_id = p.id and f.outcome = 'worked')::bigint as worked,
  (select count(*) from fix_feedback f join issues i on i.id = f.issue_id where i.project_id = p.id and f.outcome = 'failed')::bigint as failed
from projects p join orgs o on o.id = p.org_id
order by events_24h desc, p.id
limit 200;

-- name: AdminQueues :many
select queue, state::text as state, count(*)::bigint as jobs
from river_job
where state in ('available', 'running', 'retryable', 'scheduled') or (state = 'discarded' and finalized_at > now() - interval '24 hours')
group by queue, state order by queue, state;

-- name: AdminOldestAvailable :many
select queue, extract(epoch from now() - min(scheduled_at))::float8 as lag_seconds
from river_job where state = 'available' group by queue;

-- name: AdminSuggestionStats :many
-- Feedback is aggregated per suggestion first, so costs aren't multiplied by
-- the number of answers.
with fb as (
  select suggestion_id,
    count(*) filter (where outcome = 'worked') as worked,
    count(*) filter (where outcome = 'failed') as failed
  from fix_feedback group by suggestion_id
)
select s.origin, coalesce(s.catalog_key, '')::text as catalog_key,
  count(*)::bigint as suggestions,
  coalesce(sum(s.cost_usd), 0)::float8 as cost_usd,
  coalesce(sum(fb.worked), 0)::bigint as worked,
  coalesce(sum(fb.failed), 0)::bigint as failed
from fix_suggestions s
left join fb on fb.suggestion_id = s.id
group by s.origin, coalesce(s.catalog_key, '')
order by suggestions desc;

-- name: AdminIssueRaw :one
select row_to_json(i)::text from issues i where i.id = $1;

-- name: AdminQuarantined :many
select id, signature, catalog_key, worked, failed, confidence::float8 as confidence, quarantined_at
from knowledge_entries where quarantined_at is not null order by quarantined_at desc limit 50;

-- name: AdminUnquarantine :execrows
update knowledge_entries set quarantined_at = null, worked = 0, failed = 0, confidence = 0.5 where id = $1;

-- name: AdminGetProject :one
-- Staff read-only view of any org's project (same columns as GetOrgProject).
select p.id, p.name, p.target_agent, p.backend, p.allowed_domains, p.key_pages,
       coalesce((select k.public_key from project_keys k
                 where k.project_id = p.id and k.revoked_at is null
                 order by k.created_at desc limit 1), '')::text as public_key,
       o.name as org_name
from projects p join orgs o on o.id = p.org_id where p.id = $1;

