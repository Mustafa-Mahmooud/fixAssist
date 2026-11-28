-- Page-speed regressions as issues (internal/perf).

-- name: PerfIssueByFingerprint :one
select * from issues where project_id = @project_id and fingerprint = @fingerprint;

-- name: InsertPerfIssue :one
insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at,
                    event_count, user_count, impact_score)
values (@project_id, @fingerprint, 'perf', @fingerprint, @title, @route, 'new', @now, @now, @slow_visits, 0, @impact)
returning *;

-- name: ReopenPerfIssue :one
-- A fixed slowdown is back: same issue, next attempt.
update issues set status = 'open', regressed_at = @now, regression_count = regression_count + 1,
  fixed_at = null, probably_fixed_at = null, last_seen_at = @now, title = @title,
  event_count = @slow_visits, impact_score = @impact
where id = @id and status = 'fixed'
returning *;

-- name: TouchPerfIssue :exec
update issues set last_seen_at = @now, title = @title, event_count = @slow_visits, impact_score = @impact
where id = @id;

-- name: UpsertPerfRegression :exec
insert into perf_regressions (issue_id, project_id, route, metric, device, baseline_p75, baseline_n, current_p75, current_n,
                              current_release, since, element, source, breakdown, detected_at, updated_at)
values (@issue_id, @project_id, @route, @metric, @device, @baseline_p75, @baseline_n, @current_p75, @current_n,
        @current_release, @since, @element, @source, @breakdown, @now, @now)
on conflict (issue_id) do update set
  route = excluded.route, metric = excluded.metric, device = excluded.device,
  baseline_p75 = excluded.baseline_p75, baseline_n = excluded.baseline_n,
  current_p75 = excluded.current_p75, current_n = excluded.current_n,
  current_release = excluded.current_release, since = excluded.since,
  element = excluded.element, source = excluded.source, breakdown = excluded.breakdown,
  updated_at = excluded.updated_at;

-- name: GetPerfRegression :one
select * from perf_regressions where issue_id = $1;

-- name: PerfIssuesToVerify :many
-- Open slowdowns with a fix prompt: fixed once the page is fast again on
-- data from after the prompt.
select i.id, i.project_id, i.status, r.route, r.metric, r.device, r.baseline_p75,
  (select max(s.created_at) from fix_suggestions s where s.issue_id = i.id)::timestamptz as suggested_at
from issues i join perf_regressions r on r.issue_id = i.id
where i.kind = 'perf' and i.status in ('explained', 'verifying', 'open');

-- name: SeePerfCandidate :one
-- When this regression was first seen (now, if it's new).
insert into perf_candidates (project_id, route, metric, first_seen_at) values (@project_id, @route, @metric, @now)
on conflict (project_id, route, metric) do update set first_seen_at = perf_candidates.first_seen_at
returning first_seen_at;

-- name: DropPerfCandidate :exec
delete from perf_candidates where project_id = @project_id and route = @route and metric = @metric;

-- name: MarkPerfFixed :execrows
-- The page is fast again on data from after the fix prompt.
update issues set status = 'fixed', fixed_at = @now, fixed_release_id = nullif(@release::text, ''), probably_fixed_at = null
where id = @id and kind = 'perf' and status in ('explained', 'verifying', 'open');
