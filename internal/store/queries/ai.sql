-- name: GetIssueForAnalysis :one
select i.id, i.project_id, i.kind, i.fingerprint, i.signature, i.title, i.route, i.status,
       i.first_seen_at, i.last_seen_at, i.event_count, i.user_count, i.first_release_id,
       i.regression_count, i.impact_score,
       p.name as project_name, p.target_agent, p.backend, p.key_pages
from issues i join projects p on p.id = i.project_id
where i.id = $1;

-- name: IssueRecentStats :one
select coalesce(sum(events), 0)::bigint as events_24h
from issue_hourly where issue_id = $1 and hour > $2;

-- name: LatestSuggestion :one
select * from fix_suggestions where issue_id = $1 order by created_at desc, id desc limit 1;

-- name: InsertSuggestion :one
insert into fix_suggestions (issue_id, origin, catalog_key, prompt_version, target_agent, severity, title,
  what_broke, who_is_affected, why, fix_prompt, how_to_check, confidence, attempt, kb_entry_id, code_refs)
values (@issue_id, @origin, @catalog_key, @prompt_version, @target_agent, @severity, @title,
  @what_broke, @who_is_affected, @why, @fix_prompt, @how_to_check, @confidence, @attempt, @kb_entry_id, @code_refs)
returning id;

-- name: MarkIssueExplained :exec
-- A regressed issue stays 'open' ("came back") with its new second-attempt prompt.
update issues set status = case when status in ('new', 'analysis_failed') then 'explained' else status end,
  explained_at = now()
where id = $1;

-- name: UpsertKnowledgeEntry :one
-- $1 is the issue signature; the entry is keyed by kb_signature($1).
insert into knowledge_entries (signature, error_type, framework, catalog_key, explanation_template, prompts_by_platform)
values (kb_signature($1), $2, $3, $4, $5, $6)
on conflict (signature) do update set signature = excluded.signature
returning id, worked, failed, confidence::float8 as confidence, quarantined_at;

-- name: AddKBSighting :exec
insert into kb_sightings (entry_id, project_hash) values ($1, $2) on conflict do nothing;

-- name: KBTrackRecord :one
select k.worked, k.failed, k.confidence::float8 as confidence, k.quarantined_at,
  (select count(*) from kb_sightings s where s.entry_id = k.id)::bigint as apps
from knowledge_entries k where k.id = $1;

-- name: KBSimilar :one
-- Aggregates only: other apps' signatures (their table names) are never shown.
select count(*)::bigint as entries, coalesce(sum(worked), 0)::bigint as worked, coalesce(sum(failed), 0)::bigint as failed
from knowledge_entries
where signature % kb_signature(@signature::text) and similarity(signature, kb_signature(@signature::text)) >= @min_similarity::float8
  and signature <> kb_signature(@signature::text);

-- name: SuggestionsForIssue :many
select id, origin, catalog_key, attempt, created_at from fix_suggestions
where issue_id = $1 order by created_at desc limit 10;

-- Alerts -----------------------------------------------------------------

-- name: ProjectRecipients :many
select u.email from memberships m join users u on u.id = m.user_id
join projects p on p.org_id = m.org_id
where p.id = $1 order by u.id;

-- name: CountInstantAlertsToday :one
select count(*) from alert_log where project_id = $1 and kind = 'instant' and day = $2;

-- name: InsertAlert :one
insert into alert_log (project_id, issue_id, kind, day, subject, body, emailed_to)
values ($1, $2, $3, $4, $5, $6, $7)
on conflict do nothing
returning id;

-- name: SetAlertEmailed :exec
update alert_log set emailed_to = $2 where id = $1;

-- name: ListAlerts :many
select a.id, a.kind, a.subject, a.sent_at, a.issue_id, a.emailed_to
from alert_log a where a.project_id = @project_id order by a.sent_at desc
limit @lim offset @off;

-- name: CountAlerts :one
select count(*) from alert_log where project_id = $1;

-- name: ProjectsForDigest :many
select id, name, timezone from projects order by id;

-- name: DigestNewIssues :many
select i.id, i.title, i.user_count, i.event_count, i.route, i.status,
       (select s.severity from fix_suggestions s where s.issue_id = i.id order by s.created_at desc limit 1)::text as severity
from issues i
where i.project_id = $1 and i.first_seen_at >= $2 and i.status <> 'ignored'
order by i.impact_score desc, i.last_seen_at desc limit 20;

-- name: DigestOpenCount :one
select count(*) from issues where project_id = $1 and status not in ('fixed', 'ignored');

-- name: DigestFixedSince :one
select count(*) from issues where project_id = $1 and status = 'fixed' and fixed_at >= $2;

-- name: DigestPageviews :one
select count(*) from pageviews where project_id = $1 and ts >= $2 and ts < $3;

-- name: SetSuggestionModel :exec
-- What our own model cost for a suggestion it wrote (origin model).
update fix_suggestions set model = @model, tokens_in = @tokens_in, tokens_out = @tokens_out,
  cost_usd = @cost_usd, latency_ms = @latency_ms
where id = @id;
