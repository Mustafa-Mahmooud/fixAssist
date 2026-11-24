-- Tables the spike app breaks on purpose.

-- email is too short on purpose: inserts of long emails fail with 22001.
create table public.profiles (
  id bigint generated always as identity primary key,
  email varchar(20) not null,
  created_at timestamptz not null default now()
);

-- RLS enabled with no policy: every anon insert fails with 42501.
create table public.secrets (
  id bigint generated always as identity primary key,
  note text not null
);
alter table public.secrets enable row level security;

grant insert, select on public.profiles, public.secrets to anon;
