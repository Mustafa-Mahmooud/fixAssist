-- Phase 4 fix check: the spike app's database in its BROKEN state, served by
-- a local PostgREST (the same server Supabase uses for /rest/v1), so the real
-- error bodies (22001, 42501, 23505, ...) come from Postgres itself.
-- Reset before every run: everything here is recreated.

drop schema if exists public cascade;
drop schema if exists auth cascade;
create schema public;
create schema auth;

do $$
begin
  if not exists (select from pg_roles where rolname = 'anon') then create role anon nologin; end if;
  if not exists (select from pg_roles where rolname = 'authenticated') then create role authenticated nologin; end if;
  if not exists (select from pg_roles where rolname = 'authenticator') then create role authenticator login noinherit; end if;
end $$;
grant anon, authenticated to authenticator;
grant usage on schema public, auth to anon, authenticated;

-- Supabase-compatible stubs: auth.users (for user_id references) and
-- auth.uid() (used in RLS policies).
create table auth.users (id uuid primary key);

create function auth.uid() returns uuid language sql stable as $$
  select nullif(current_setting('request.jwt.claim.sub', true), '')::uuid
$$;

-- email is too short on purpose: long emails fail with 22001.
create table public.profiles (
  id bigint generated always as identity primary key,
  email varchar(20) not null,
  created_at timestamptz not null default now()
);

-- RLS on with no policy: every anonymous insert fails with 42501.
create table public.secrets (
  id bigint generated always as identity primary key,
  note text not null
);
alter table public.secrets enable row level security;

-- New cases: unique email (23505), required name (23502), uuid id (22P02),
-- single-row lookups (PGRST116).
create table public.members (
  id uuid primary key default gen_random_uuid(),
  email text not null unique,
  name text not null,
  created_at timestamptz not null default now()
);

grant select, insert, update on public.profiles, public.secrets, public.members to anon, authenticated;

notify pgrst, 'reload schema';
