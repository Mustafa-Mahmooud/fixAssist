-- +goose Up

-- Fine-tuning data for our own model. One row per suggestion: the scrubbed
-- evidence the model sees (input), what we answered (output, any origin) and,
-- later, what the founder's agent changed and whether it worked. Snapshotted
-- at suggestion time, so it outlives the 30-day event retention. Projects can
-- opt out (settings); opting out deletes their rows.
alter table projects add column training_opt_out boolean not null default false;

create table training_examples (
  id bigint generated always as identity primary key,
  project_id bigint not null references projects(id) on delete cascade,
  issue_id bigint not null references issues(id) on delete cascade,
  suggestion_id bigint not null unique references fix_suggestions(id) on delete cascade,
  signature text not null,
  origin text not null,
  catalog_key text,
  model text,
  prompt_version text not null,
  attempt integer not null,
  input text not null,
  output jsonb not null,
  agent_summary text not null default '',
  files text[] not null default '{}',
  outcome text check (outcome in ('worked', 'failed', 'partial')),
  outcome_source text,
  created_at timestamptz not null default now(),
  outcome_at timestamptz
);
create index training_examples_created on training_examples (created_at);
create index training_examples_signature on training_examples (signature, created_at);

-- +goose Down
drop table training_examples;
alter table projects drop column training_opt_out;
