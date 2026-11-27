-- +goose Up

-- One-time pairing codes: the Setup page shows one inside the install
-- command, and fixassist trades it for an agent token, so founders never
-- copy a token by hand. Stored hashed; valid 15 minutes; used once.
create table pair_codes (
  code_hash  text primary key,
  project_id bigint not null references projects(id) on delete cascade,
  created_by bigint references users(id) on delete set null,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at    timestamptz,
  token_id   bigint references agent_tokens(id) on delete set null
);
create index pair_codes_project on pair_codes (project_id, created_at desc);

-- +goose Down
drop table pair_codes;
