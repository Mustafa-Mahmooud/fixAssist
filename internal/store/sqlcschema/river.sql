-- Typing only, for sqlc: River creates the real table with its own migrations
-- (rivermigrate in store.Migrate). Keep the columns we query in sync.
create type river_job_state as enum ('available', 'cancelled', 'completed', 'discarded', 'pending', 'retryable', 'running', 'scheduled');
create table river_job (
  id bigint primary key,
  state river_job_state not null,
  queue text not null,
  kind text not null,
  scheduled_at timestamptz not null,
  finalized_at timestamptz
);
