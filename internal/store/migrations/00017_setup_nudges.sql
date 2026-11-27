-- +goose Up

-- Setup reminders: one email when an app has had no visit a day after it
-- was added (with what "Check my site" found), and one when it is connected
-- but has caught nothing after three days (with a test-error link). Each is
-- sent at most once per app.
alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check
  check (kind in ('instant', 'digest', 'quota80', 'quota100', 'growth', 'autofix', 'setup_install', 'setup_test'));
create unique index alert_log_setup_once on alert_log (project_id, kind) where kind in ('setup_install', 'setup_test');

-- +goose Down
drop index alert_log_setup_once;
delete from alert_log where kind in ('setup_install', 'setup_test');
alter table alert_log drop constraint alert_log_kind_check;
alter table alert_log add constraint alert_log_kind_check
  check (kind in ('instant', 'digest', 'quota80', 'quota100', 'growth', 'autofix'));
