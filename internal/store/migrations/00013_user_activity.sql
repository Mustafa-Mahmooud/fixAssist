-- +goose Up

-- Admin analytics: when each person last used the dashboard, and on which
-- days (for daily/weekly active users). Written at most every 15 minutes
-- per person, from session loading.
alter table users add column last_seen_at timestamptz;
create table user_activity_days (
  user_id bigint not null references users(id) on delete cascade,
  day date not null,
  primary key (user_id, day)
);
create index user_activity_days_day on user_activity_days (day);

-- +goose Down
drop table user_activity_days;
alter table users drop column last_seen_at;
