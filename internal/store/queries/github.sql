-- name: GetProjectGitHub :one
select project_id, installation_id, repo_full_name from project_github where project_id = $1;

-- name: ConnectProjectGitHub :exec
insert into project_github (project_id, installation_id, connected_by) values ($1, $2, $3)
on conflict (project_id) do update set installation_id = excluded.installation_id, repo_full_name = null,
  connected_by = excluded.connected_by, connected_at = now();

-- name: SetProjectRepo :execrows
update project_github set repo_full_name = $2 where project_id = $1;

-- name: DisconnectProjectGitHub :exec
delete from project_github where project_id = $1;
