-- +goose Up

-- Server-side errors: events sent by a Sentry server SDK (Node, Python, PHP,
-- Ruby, Java, .NET, Go, ...). Their releases are stored as "server:<release>"
-- so a backend deploy and a frontend deploy are never confused when
-- verifying fixes.
alter table events drop constraint events_kind_check;
alter table events add constraint events_kind_check check (kind in ('js', 'net', 'message', 'server'));
alter table issues drop constraint issues_kind_check;
alter table issues add constraint issues_kind_check check (kind in ('js', 'net', 'message', 'server'));

-- +goose Down
delete from issues where kind = 'server';
delete from events where kind = 'server';
alter table events drop constraint events_kind_check;
alter table events add constraint events_kind_check check (kind in ('js', 'net', 'message'));
alter table issues drop constraint issues_kind_check;
alter table issues add constraint issues_kind_check check (kind in ('js', 'net', 'message'));
