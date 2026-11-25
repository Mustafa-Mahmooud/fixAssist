-- Agent loop (MCP) ----------------------------------------------------------

-- name: CreateAgentToken :one
insert into agent_tokens (project_id, name, token_hash, prefix, created_by)
values (@project_id, @name, @token_hash, @prefix, @created_by)
returning id, created_at;

-- name: AgentTokenByHash :one
-- A live token and its project.
select t.id, t.project_id, p.target_agent, p.name as project_name
from agent_tokens t join projects p on p.id = t.project_id
where t.token_hash = $1 and t.revoked_at is null;

-- name: TouchAgentToken :exec
update agent_tokens set last_used_at = now()
where id = $1 and (last_used_at is null or last_used_at < now() - interval '1 minute');

-- name: ListAgentTokens :many
select id, name, prefix, created_at, last_used_at from agent_tokens
where project_id = $1 and revoked_at is null order by created_at desc;

-- name: RevokeAgentToken :execrows
update agent_tokens set revoked_at = now() where id = @id and project_id = @project_id and revoked_at is null;

-- name: AgentIssues :many
-- Open work first, most impactful first.
select id, kind, title, route, status, event_count, user_count, last_seen_at, regression_count
from issues
where project_id = @project_id
  and (@include_all::boolean or status in ('new', 'explained', 'open', 'verifying'))
order by (status in ('new', 'explained', 'open')) desc, impact_score desc, last_seen_at desc
limit 50;

-- name: InsertFixAttempt :one
insert into fix_attempts (issue_id, suggestion_id, attempt, source, agent_token_id, release_id, summary, files, reported_at)
values (@issue_id, @suggestion_id, @attempt, @source, @agent_token_id, @release_id, @summary, @files, @reported_at)
returning id;

-- name: FixAttempts :many
select * from fix_attempts where issue_id = $1 order by reported_at, id;

-- name: LatestFixAttempt :one
select * from fix_attempts where issue_id = $1 order by reported_at desc, id desc limit 1;

-- name: CloseLatestAttempt :exec
-- The latest attempt still waiting for an outcome gets one.
update fix_attempts set outcome = @outcome, outcome_at = now()
where id = (select a.id from fix_attempts a where a.issue_id = @issue_id and a.outcome is null order by a.reported_at desc, a.id desc limit 1);

-- name: MarkAttemptReproduced :exec
update fix_attempts set reproduced_at = @at where id = @id;

-- name: ReleaseFirstSeen :one
select first_seen_at from releases where project_id = @project_id and release_id = @release_id;

-- name: LastEventSince :one
select max(received_at)::timestamptz from events
where project_id = @project_id and fingerprint = @fingerprint and received_at >= @since;
