-- Auto-fix (internal/autofix) ----------------------------------------------

-- name: ProjectAutofix :one
select autofix_enabled, autofix_rule from projects where id = $1;

-- name: SetProjectAutofix :exec
update projects set autofix_enabled = @enabled, autofix_rule = @rule where id = @id;

-- name: TouchDaemon :exec
insert into agent_daemons (token_id, project_id, hostname, version, last_seen_at)
values (@token_id, @project_id, @hostname, @version, now())
on conflict (token_id) do update
  set hostname = excluded.hostname, version = excluded.version, last_seen_at = now();

-- name: ProjectDaemons :many
select d.token_id, d.hostname, d.version, d.last_seen_at, t.name as token_name
from agent_daemons d join agent_tokens t on t.id = d.token_id
where d.project_id = $1 and t.revoked_at is null
order by d.last_seen_at desc;

-- name: DaemonOnline :one
-- Online: a live token's daemon polled within the window.
select exists (
  select 1 from agent_daemons d join agent_tokens t on t.id = d.token_id
  where d.project_id = @project_id and t.revoked_at is null and d.last_seen_at > @since
)::boolean;

-- name: InsertAutofixRun :one
-- Nothing is inserted while another run for the issue is in progress.
insert into autofix_runs (project_id, issue_id, suggestion_id, state, approved_by, approved_user_id, approved_at)
values (@project_id, @issue_id, @suggestion_id, @state, sqlc.narg(approved_by), sqlc.narg(approved_user_id), sqlc.narg(approved_at))
on conflict do nothing
returning *;

-- name: GetAutofixRun :one
select * from autofix_runs where id = @id and project_id = @project_id;

-- name: GetAutofixRunByID :one
select * from autofix_runs where id = $1;

-- name: LatestAutofixRun :one
select * from autofix_runs where issue_id = $1 order by created_at desc, id desc limit 1;

-- name: ListAutofixRuns :many
select r.*, i.title as issue_title from autofix_runs r join issues i on i.id = r.issue_id
where r.project_id = $1 order by r.created_at desc, r.id desc limit 20;

-- name: ApproveAutofixRun :one
update autofix_runs
set state = 'approved', approved_by = 'user', approved_user_id = @user_id, approved_at = now(), updated_at = now()
where id = @id and project_id = @project_id and state = 'pending_approval'
returning *;

-- name: RejectAutofixRun :one
update autofix_runs set state = 'rejected', updated_at = now()
where id = @id and project_id = @project_id and state in ('pending_approval', 'approved')
returning *;

-- name: ApproveAutofixMerge :one
update autofix_runs
set state = 'merge_approved', approved_user_id = @user_id, updated_at = now()
where id = @id and project_id = @project_id and state = 'awaiting_merge_approval'
returning *;

-- name: ClaimedAutofixRuns :one
-- Runs claimed in the window (the daily cap) and in progress right now
-- (one at a time).
select count(*) filter (where claimed_at > @since)::bigint as today,
       count(*) filter (where state = 'claimed')::bigint as running
from autofix_runs where project_id = @project_id;

-- name: NextApprovedAutofixRun :one
select * from autofix_runs
where project_id = @project_id and state = 'approved'
order by approved_at, id
limit 1
for update skip locked;

-- name: ClaimAutofixRun :one
update autofix_runs
set state = 'claimed', token_id = @token_id, claimed_at = now(), lease_until = @lease_until, updated_at = now()
where id = @id and state = 'approved'
returning *;

-- name: NextMergeApprovedRun :one
-- A merge approval for the daemon: it turns on auto-merge. Taken once.
update autofix_runs set state = 'pr_opened', auto_merge = true, updated_at = now()
where id = (
  select r.id from autofix_runs r
  where r.project_id = @project_id and r.state = 'merge_approved'
  order by r.updated_at, r.id limit 1
  for update skip locked)
returning *;

-- name: OpenAutofixPRs :many
-- Pull requests the daemon still has to watch until merged or closed.
select * from autofix_runs
where project_id = $1 and state in ('pr_opened', 'awaiting_merge_approval', 'merge_approved') and pr_url <> ''
order by id;

-- name: SetAutofixPR :one
update autofix_runs
set state = @state, branch = @branch, pr_url = @pr_url, auto_merge = @auto_merge,
    summary = @summary, files = @files, lease_until = null, error = '', updated_at = now()
where id = @id and project_id = @project_id and state = 'claimed'
returning *;

-- name: SetAutofixMerged :one
update autofix_runs set state = 'merged', lease_until = null, updated_at = now()
where id = @id and project_id = @project_id and state in ('pr_opened', 'awaiting_merge_approval', 'merge_approved')
returning *;

-- name: SetAutofixFailed :one
update autofix_runs set state = 'failed', error = @error, lease_until = null, updated_at = now()
where id = @id and project_id = @project_id
  and state in ('claimed', 'pr_opened', 'awaiting_merge_approval', 'merge_approved')
returning *;

-- name: SetAutofixNote :exec
-- A problem that doesn't end the run (auto-merge couldn't be turned on).
update autofix_runs set error = @error, updated_at = now() where id = @id and project_id = @project_id;

-- name: ExtendAutofixLease :execrows
update autofix_runs set lease_until = @lease_until, updated_at = now()
where id = @id and project_id = @project_id and state = 'claimed';

-- name: FailExpiredAutofixLeases :many
-- The daemon stopped mid-run. Not retried on its own: the founder can
-- approve again.
update autofix_runs
set state = 'failed', error = 'The daemon stopped before it finished.', lease_until = null, updated_at = now()
where state = 'claimed' and lease_until < @now
returning id, project_id, issue_id;

-- name: ExpireStaleAutofixApprovals :execrows
update autofix_runs set state = 'expired', updated_at = now()
where state = 'pending_approval' and created_at < @before;

-- name: ApprovedAutofixRunsWaiting :many
-- Approved runs whose project has had no daemon online since @since.
select r.* from autofix_runs r
where r.state = 'approved'
  and exists (select 1 from projects p where p.id = r.project_id and p.autofix_enabled)
  and not exists (
    select 1 from agent_daemons d join agent_tokens t on t.id = d.token_id
    where d.project_id = r.project_id and t.revoked_at is null and d.last_seen_at > @since)
order by r.id
limit 100;

-- name: MarkAutofixEmailed :execrows
update autofix_runs set state = 'emailed', updated_at = now() where id = $1 and state = 'approved';

-- name: InstantAlertedSince :one
-- Whether the issue's fix prompt was already emailed (instant alert).
select exists (
  select 1 from alert_log where issue_id = @issue_id and kind = 'instant' and sent_at > @since
)::boolean;

-- name: CountAutofixAlertsSince :one
select count(*) from alert_log where project_id = @project_id and kind = 'autofix' and sent_at > @since;

-- name: SuggestionByID :one
select * from fix_suggestions where id = $1;
