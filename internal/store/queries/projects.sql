-- name: CreateOrg :one
insert into orgs (name) values ($1)
returning id;

-- name: GetOrgByName :one
select id from orgs where name = $1 order by id limit 1;

-- name: CreateProject :one
insert into projects (org_id, name, target_agent, backend, stack_tags, allowed_domains, quota_monthly, hmac_secret)
values ($1, $2, $3, $4, $5, $6, $7, $8)
returning id, created_at;

-- name: CreateProjectKey :exec
insert into project_keys (public_key, project_id) values ($1, $2);

-- name: RevokeProjectKeys :execrows
update project_keys set revoked_at = now()
where project_id = $1 and revoked_at is null;

-- name: ListProjects :many
select p.id, p.name, o.name as org_name, p.target_agent, p.backend, p.allowed_domains, p.quota_monthly,
       coalesce((select k.public_key from project_keys k
                 where k.project_id = p.id and k.revoked_at is null
                 order by k.created_at desc limit 1), '')::text as public_key
from projects p join orgs o on o.id = p.org_id
order by p.id;

-- name: ListActiveKeys :many
select k.public_key, p.id as project_id, p.target_agent, p.backend, p.allowed_domains, p.quota_monthly, p.hmac_secret
from project_keys k join projects p on p.id = k.project_id
where k.revoked_at is null;

-- name: GetActiveKey :one
select k.public_key, p.id as project_id, p.target_agent, p.backend, p.allowed_domains, p.quota_monthly, p.hmac_secret
from project_keys k join projects p on p.id = k.project_id
where k.public_key = $1 and k.revoked_at is null;

-- name: SetProjectQuota :exec
update projects set quota_monthly = $2 where id = $1;

-- name: ListUsageForMonth :many
select project_id, events from project_usage_monthly where month = $1;
