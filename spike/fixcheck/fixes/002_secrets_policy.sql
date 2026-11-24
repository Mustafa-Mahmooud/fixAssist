-- net_42501 prompt: let logged-in users add only their own rows; link rows
-- to users; keep RLS on and give anonymous visitors nothing.
alter table public.secrets add column user_id uuid references auth.users default auth.uid();
create policy "Users can add their own secrets" on public.secrets
  for insert to authenticated
  with check (user_id = auth.uid());
notify pgrst, 'reload schema';
