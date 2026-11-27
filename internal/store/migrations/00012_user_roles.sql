-- +goose Up

-- Roles: 'admin' opens the staff area (/admin, every account read-only).
-- Emails in STAFF_EMAILS are always admin on top of this and can't be
-- demoted from the app, so a bad write can't lock everyone out.
alter table users add column role text not null default 'user' check (role in ('user', 'admin'));

-- +goose Down
alter table users drop column role;
