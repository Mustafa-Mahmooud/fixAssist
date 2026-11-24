-- name: LockEventForGrouping :one
select e.id, e.received_at, e.project_id, e.kind, e.fingerprint, e.release_id, e.route, e.user_hash, e.payload, e.grouped_at,
       p.target_agent, p.backend, p.key_pages
from events e join projects p on p.id = e.project_id
where e.id = $1 and e.received_at = $2
for update of e;

-- name: UpsertIssue :one
insert into issues (project_id, fingerprint, kind, signature, title, route,
                    first_seen_at, last_seen_at, event_count, first_release_id, last_release_id)
values (@project_id, @fingerprint, @kind, @signature, @title, @route,
        @seen_at, @seen_at, 1, @release_id, @release_id)
on conflict (project_id, fingerprint) do update set
  -- Events are grouped in parallel and can arrive out of order, so each
  -- release follows its timestamp rather than the order of grouping.
  first_release_id = case when excluded.first_seen_at < issues.first_seen_at
                          then excluded.first_release_id else issues.first_release_id end,
  first_seen_at = least(issues.first_seen_at, excluded.first_seen_at),
  last_release_id = case when excluded.last_seen_at >= issues.last_seen_at
                         then coalesce(excluded.last_release_id, issues.last_release_id) else issues.last_release_id end,
  last_seen_at = greatest(issues.last_seen_at, excluded.last_seen_at),
  event_count = issues.event_count + 1
returning id, status, fixed_at, fixed_release_id, watch_since, event_count, user_count, (xmax = 0)::boolean as inserted;

-- name: InsertIssueUser :execrows
insert into issue_users (issue_id, user_hash, first_seen_at, last_seen_at)
values ($1, $2, $3, $3)
on conflict do nothing;

-- name: TouchIssueUserNewHour :execrows
update issue_users set last_seen_at = @seen_at
where issue_id = @issue_id and user_hash = @user_hash and last_seen_at < @hour_start;

-- name: TouchIssueUser :exec
update issue_users set last_seen_at = greatest(last_seen_at, @seen_at)
where issue_id = @issue_id and user_hash = @user_hash;

-- name: BumpIssueUserCount :one
update issues set user_count = user_count + 1 where id = $1
returning user_count;

-- name: CountRecentIssueUsers :one
select count(*) from issue_users where issue_id = $1 and last_seen_at > $2;

-- name: BumpIssueHourly :exec
insert into issue_hourly (issue_id, hour, events, users)
values (@issue_id, @hour, 1, @new_users)
on conflict (issue_id, hour) do update set
  events = issue_hourly.events + 1,
  users = issue_hourly.users + excluded.users;

-- name: InsertIssueSample :exec
insert into issue_samples (issue_id, event_id, received_at, release_id, payload)
values ($1, $2, $3, $4, $5);

-- name: TrimIssueSamples :exec
delete from issue_samples s
where s.issue_id = @issue_id and s.id not in (
  select k.id from issue_samples k where k.issue_id = @issue_id
  order by k.received_at desc, k.id desc limit @keep
);

-- name: UpsertRelease :one
insert into releases (project_id, release_id, first_seen_at, last_seen_at, event_count)
values ($1, $2, $3, $3, 1)
on conflict (project_id, release_id) do update set
  last_seen_at = greatest(releases.last_seen_at, excluded.last_seen_at),
  event_count = releases.event_count + 1
returning first_seen_at, (xmax = 0)::boolean as inserted;

-- name: ReopenIssue :execrows
update issues set status = 'open', regressed_at = @at, regression_count = regression_count + 1
where id = @id and status in ('fixed', 'verifying');

-- name: SetIssueImpact :exec
update issues set impact_score = $2 where id = $1;

-- name: MarkEventIssue :exec
update events set issue_id = @issue_id, grouped_at = now()
where id = @id and received_at = @received_at;

-- name: MarkIssueFixed :execrows
update issues set status = 'fixed', fixed_at = now(), fixed_release_id = @release_id,
  watch_since = coalesce((select r.first_seen_at from releases r where r.project_id = @project_id and r.release_id = @release_id), now())
where id = @id and project_id = @project_id;
