-- +goose Up

-- GitHub App code context (FDR Phase 7): which repo a project's code lives in.
-- The installation was verified to belong to the connecting user.
create table project_github (
  project_id bigint primary key references projects(id) on delete cascade,
  installation_id bigint not null,
  repo_full_name text,                 -- owner/name; null until chosen
  connected_by bigint references users(id) on delete set null,
  connected_at timestamptz not null default now()
);

-- +goose Down
drop table project_github;
