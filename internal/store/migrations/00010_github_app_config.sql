-- +goose Up

-- The GitHub App's own credentials, saved by the manifest flow on
-- /admin/github so hosted deployments (Northflank, ...) don't need its
-- multi-line private key copied into environment variables. One row.
-- The client secret and private key are encrypted (AES-256-GCM, key derived
-- from APP_SECRET); the environment variables still win when set.
create table github_app_config (
  id integer primary key default 1 check (id = 1),
  app_id bigint not null,
  slug text not null,
  client_id text not null,
  client_secret_enc bytea not null,
  private_key_enc bytea not null,
  html_url text,
  created_at timestamptz not null default now()
);

-- +goose Down
drop table github_app_config;
