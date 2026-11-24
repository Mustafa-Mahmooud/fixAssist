-- +goose Up

-- Phase 7 (agent mode): cross-customer knowledge reuse instead of an LLM
-- cache. Entries are keyed by the signature without the per-app platform
-- and stack tags, so the same error in different apps shares one track record.
create extension if not exists pg_trgm;

-- +goose StatementBegin
create function kb_signature(sig text) returns text
language sql immutable parallel safe as $$
  select btrim(regexp_replace(sig, '\s+(platform|stack)=\S+', '', 'g'))
$$;
-- +goose StatementEnd

-- Merge existing entries that now share a key (sum their counts).
with g as (
  select kb_signature(signature) as k, min(id) as keep, sum(worked) as w, sum(failed) as f
  from knowledge_entries group by 1
)
update knowledge_entries e set worked = g.w, failed = g.f from g where e.id = g.keep;
delete from knowledge_entries e using (
  select id, min(id) over (partition by kb_signature(signature)) as keep from knowledge_entries
) d where e.id = d.id and d.id <> d.keep;
update knowledge_entries set signature = kb_signature(signature),
  confidence = round((worked + 1)::numeric / (worked + failed + 2), 3);

-- Quarantine: a fix pattern that keeps failing stops being offered.
alter table knowledge_entries add column quarantined_at timestamptz;
create index knowledge_entries_trgm on knowledge_entries using gin (signature gin_trgm_ops);

-- How many different apps hit an entry (hashed project ids, no customer data).
create table kb_sightings (
  entry_id bigint not null references knowledge_entries(id) on delete cascade,
  project_hash text not null,
  primary key (entry_id, project_hash)
);

-- Which entry (and so which proven or quarantined pattern) a suggestion used,
-- and file-level code references from the GitHub App.
alter table fix_suggestions
  add column kb_entry_id bigint references knowledge_entries(id) on delete set null,
  add column code_refs jsonb;

-- +goose Down
alter table fix_suggestions drop column code_refs, drop column kb_entry_id;
drop table kb_sightings;
drop index knowledge_entries_trgm;
alter table knowledge_entries drop column quarantined_at;
drop function kb_signature(text);
