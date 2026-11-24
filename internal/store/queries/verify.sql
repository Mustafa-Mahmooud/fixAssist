-- name: IssuesToVerify :many
select i.id, i.project_id, i.status, i.route, i.fingerprint, i.watch_since, i.verify_release_id, i.probably_fixed_at,
       s.created_at as suggested_at
from issues i
join lateral (select created_at from fix_suggestions f where f.issue_id = i.id order by f.created_at desc limit 1) s on true
where i.status in ('explained', 'verifying');

-- name: FirstReleaseSince :one
select release_id, first_seen_at from releases
where project_id = $1 and first_seen_at > $2
order by first_seen_at limit 1;

-- name: LatestRelease :one
select release_id, first_seen_at from releases
where project_id = $1 order by first_seen_at desc limit 1;

-- name: StartVerifying :execrows
update issues set status = 'verifying', verify_release_id = @release_id, watch_since = @watch_since, probably_fixed_at = null
where id = @id and status in ('explained', 'open');

-- name: StartVerifyingManual :execrows
-- "I applied the fix": watch the next release that appears.
update issues set status = 'verifying', verify_release_id = null, watch_since = @watch_since, probably_fixed_at = null
where id = @id and project_id = @project_id and status not in ('fixed', 'ignored');

-- name: SetVerifyRelease :exec
update issues set verify_release_id = @release_id, watch_since = @watch_since where id = @id and status = 'verifying';

-- name: SessionsOnRoute :one
select count(distinct session_id)::bigint from pageviews
where project_id = @project_id and route = @route and release_id = @release_id;

-- name: EventsOnRelease :one
select count(*)::bigint from events
where project_id = @project_id and fingerprint = @fingerprint and release_id = @release_id;

-- name: MarkVerifiedFixed :execrows
update issues set status = 'fixed', fixed_at = now(), fixed_release_id = verify_release_id,
  probably_fixed_at = null
where id = $1 and status = 'verifying';

-- name: SetProbablyFixed :exec
update issues set probably_fixed_at = now() where id = $1 and probably_fixed_at is null;

-- name: SetIssueFixedExplicit :execrows
update issues set status = 'fixed', fixed_at = now(), fixed_release_id = @release_id,
  watch_since = @watch_since, probably_fixed_at = null
where id = @id;

-- name: SetIssueOpen :execrows
update issues set status = 'open', probably_fixed_at = null where id = $1;

-- name: InsertFeedback :one
insert into fix_feedback (suggestion_id, issue_id, outcome, source, release_id, user_id)
values ($1, $2, $3, $4, $5, $6)
returning id;

-- name: UpdateKnowledgeFromFeedback :exec
-- Updates the entry whose pattern the suggestion used (none for quarantined
-- fallbacks). Confidence = (worked + 1) / (worked + failed + 2) (FDR).
-- Quarantined once it has $4 answers and confidence below $5.
-- $1 entry id, $2 worked delta, $3 failed delta.
update knowledge_entries k set
  worked = k.worked + $2::int,
  failed = k.failed + $3::int,
  confidence = round((k.worked + $2::int + 1)::numeric / (k.worked + k.failed + $2::int + $3::int + 2), 3),
  quarantined_at = case
    when k.worked + k.failed + $2::int + $3::int >= $4::int
     and (k.worked + $2::int + 1)::numeric / (k.worked + k.failed + $2::int + $3::int + 2) < $5::numeric
    then coalesce(k.quarantined_at, now()) else null end
where k.id = $1;

-- name: IssueFeedback :many
select id, outcome, source, release_id, created_at from fix_feedback
where issue_id = $1 order by created_at desc limit 10;

-- name: GetSuggestion :one
select s.*, i.project_id from fix_suggestions s join issues i on i.id = s.issue_id where s.id = $1;

-- Quotas -------------------------------------------------------------------

-- name: QuotaUsage :many
select p.id, p.name, p.quota_monthly, coalesce(u.events, 0)::bigint as events
from projects p
left join project_usage_monthly u on u.project_id = p.id and u.month = $1;
