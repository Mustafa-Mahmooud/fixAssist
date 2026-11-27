-- name: CreatePairCode :exec
insert into pair_codes (code_hash, project_id, created_by, expires_at)
values (@code_hash, @project_id, @created_by, @expires_at);

-- name: UsePairCode :one
-- Atomically claims an unused, unexpired code.
update pair_codes set used_at = now()
where code_hash = @code_hash and used_at is null and expires_at > now()
returning project_id, created_by;

-- name: SetPairCodeToken :exec
update pair_codes set token_id = @token_id where code_hash = @code_hash;

-- name: ProjectName :one
select name from projects where id = $1;
