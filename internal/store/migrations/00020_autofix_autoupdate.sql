-- +goose Up

-- Whether this app's daemon may update itself when a newer signed build is
-- deployed (internal/daemonver). On by default; a daemon serving several
-- apps updates only when all of them allow it.
alter table projects add column autofix_autoupdate boolean not null default true;

-- +goose Down
alter table projects drop column autofix_autoupdate;
