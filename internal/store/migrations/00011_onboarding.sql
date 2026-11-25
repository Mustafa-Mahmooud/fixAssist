-- +goose Up

-- Onboarding: page tips can be hidden per person, and a project's
-- "Getting started" checklist can be dismissed. Progress itself is derived
-- from the data (first page view, first issue, ...), never stored.
alter table users add column tips_hidden boolean not null default false;
alter table projects add column onboarding_dismissed_at timestamptz;

-- +goose Down
alter table projects drop column onboarding_dismissed_at;
alter table users drop column tips_hidden;
