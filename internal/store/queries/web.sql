-- Auth ---------------------------------------------------------------------

-- name: CountRecentLoginTokens :one
select count(*) from login_tokens where lower(email) = lower($1) and created_at > $2;

-- name: InsertLoginToken :exec
insert into login_tokens (token_hash, email, expires_at) values ($1, $2, $3);

-- name: UseLoginToken :one
update login_tokens set used_at = now()
where token_hash = $1 and used_at is null and expires_at > now()
returning email;

-- name: GetUserByEmail :one
select id, email from users where lower(email) = lower($1);

-- name: CreateUser :one
insert into users (email) values ($1) returning id, email;

-- name: CreateMembership :exec
insert into memberships (user_id, org_id, role) values ($1, $2, $3);

-- name: FirstOrgForUser :one
select o.id, o.name from memberships m join orgs o on o.id = m.org_id
where m.user_id = $1 order by m.created_at limit 1;

-- name: CreateWebSession :exec
insert into web_sessions (id_hash, user_id, csrf_token, expires_at) values ($1, $2, $3, $4);

-- name: GetWebSession :one
select s.user_id, s.csrf_token, s.expires_at, u.email, u.tips_hidden
from web_sessions s join users u on u.id = s.user_id
where s.id_hash = $1 and s.expires_at > now();

-- name: ExtendWebSession :exec
update web_sessions set expires_at = $2 where id_hash = $1;

-- name: DeleteWebSession :exec
delete from web_sessions where id_hash = $1;

-- name: PruneAuth :exec
with t as (delete from login_tokens where expires_at < now() - interval '1 day')
delete from web_sessions where expires_at < now();

-- Dashboard ----------------------------------------------------------------

-- name: ListOrgProjects :many
select p.id, p.name, p.target_agent, p.backend, p.created_at,
       coalesce((select k.public_key from project_keys k
                 where k.project_id = p.id and k.revoked_at is null
                 order by k.created_at desc limit 1), '')::text as public_key,
       (select count(*) from issues i where i.project_id = p.id and i.status not in ('fixed', 'ignored'))::bigint as open_issues
from projects p where p.org_id = $1 order by p.id;

-- name: GetOrgProject :one
select p.id, p.name, p.target_agent, p.backend, p.allowed_domains, p.key_pages,
       coalesce((select k.public_key from project_keys k
                 where k.project_id = p.id and k.revoked_at is null
                 order by k.created_at desc limit 1), '')::text as public_key
from projects p where p.id = $1 and p.org_id = $2;

-- name: ProjectEventStats :one
-- "Connected" means anything arrived: an error or a page view. A healthy site
-- may send only page views for a long time.
select
  (select count(*) from events e where e.project_id = @project_id and e.received_at > now() - interval '30 days')::bigint as events,
  (select count(*) from events e where e.project_id = @project_id and e.kind = 'server' and e.received_at > now() - interval '30 days')::bigint as server_events,
  (select count(*) from pageviews v where v.project_id = @project_id and v.ts > now() - interval '30 days')::bigint as page_views,
  greatest(
    (select max(e.received_at) from events e where e.project_id = @project_id and e.received_at > now() - interval '30 days'),
    (select max(v.ts) from pageviews v where v.project_id = @project_id and v.ts > now() - interval '30 days')
  )::timestamptz as last_event_at;

-- name: ListIssues :many
select id, kind, title, route, status, first_seen_at, last_seen_at, event_count, user_count,
       impact_score, last_release_id, regression_count
from issues
where project_id = @project_id
  and (@status::text = '' or status = @status::text
       or (@status::text = 'active' and status not in ('fixed', 'ignored')))
order by impact_score desc, last_seen_at desc
limit 200;

-- name: GetIssue :one
select * from issues where id = $1 and project_id = $2;

-- name: IssueHourly :many
select hour, events, users from issue_hourly
where issue_id = $1 and hour >= $2 order by hour;

-- name: IssueSamples :many
select id, received_at, release_id, payload from issue_samples
where issue_id = $1 order by received_at desc limit $2;

-- name: SetIssueStatus :execrows
update issues set status = $3 where id = $1 and project_id = $2;

-- Onboarding ---------------------------------------------------------------

-- name: ProjectOnboarding :one
-- Progress is read from the data, so it can't drift from what happened.
select
  (p.onboarding_dismissed_at is not null)::boolean as dismissed,
  (exists (select 1 from pageviews v where v.project_id = p.id)
   or exists (select 1 from events e where e.project_id = p.id))::boolean as connected,
  exists (select 1 from issues i where i.project_id = p.id)::boolean as has_issue,
  exists (select 1 from fix_suggestions f join issues i on i.id = f.issue_id where i.project_id = p.id)::boolean as explained,
  exists (select 1 from events e where e.project_id = p.id and e.kind = 'server')::boolean as server_errors,
  exists (select 1 from project_github g where g.project_id = p.id and g.repo_full_name is not null)::boolean as code_connected,
  exists (select 1 from agent_tokens t where t.project_id = p.id and t.revoked_at is null)::boolean as agent_connected,
  coalesce((select i.id from issues i where i.project_id = p.id order by i.impact_score desc, i.last_seen_at desc limit 1), 0)::bigint as first_issue_id
from projects p where p.id = $1;

-- name: DismissOnboarding :exec
update projects set onboarding_dismissed_at = now() where id = $1;

-- name: SetTipsHidden :exec
update users set tips_hidden = @hidden where id = @id;

-- Sign in with GitHub --------------------------------------------------------

-- name: GetUserByGitHubID :one
select id, email from users where github_id = $1;

-- name: LinkGitHub :execrows
-- Links an existing account (found by verified email) to its GitHub user,
-- only if it isn't linked to another one.
update users set github_id = @github_id where id = @id and (github_id is null or github_id = @github_id);

-- name: CreateUserWithGitHub :one
insert into users (email, github_id) values (@email, @github_id) returning id, email;
