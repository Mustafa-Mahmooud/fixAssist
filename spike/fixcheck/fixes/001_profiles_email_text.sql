-- net_22001 prompt: change the varchar(20) column in profiles to text.
alter table public.profiles alter column email type text;
notify pgrst, 'reload schema';
