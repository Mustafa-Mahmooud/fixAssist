-- +goose Up

-- Setup help for new founders: sites whose events were refused because
-- their address isn't in the project's allowed domains, so Setup can say
-- "we got visits from preview--myapp.lovable.app, allow it?". Ingest writes
-- at most 20 hosts per project, each at most once a minute.
create table project_rejected_origins (
  project_id    bigint not null references projects(id) on delete cascade,
  host          text not null,
  first_seen_at timestamptz not null default now(),
  last_seen_at  timestamptz not null default now(),
  hits          bigint not null default 1,
  primary key (project_id, host)
);

-- +goose Down
drop table project_rejected_origins;
