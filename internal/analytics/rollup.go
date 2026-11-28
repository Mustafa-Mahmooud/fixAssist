package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Rollups turn raw page views and analytics events (30-day retention) into
// small daily tables that are kept forever and read by every screen.
//
// Each run recomputes whole days (delete + insert in one transaction), so
// it's idempotent and late events are picked up on the next run. The hourly
// job recomputes today and yesterday.

// dayStatements rebuild one UTC day for all projects: $1 day, $2 start, $3 end.
var dayStatements = []string{
	`delete from daily_overview where day = $1`,
	`insert into daily_overview (project_id, day, visitors, sessions, pageviews, signups, logins)
	 select project_id, $1::date,
	   count(distinct visitor_hash), count(distinct session_id),
	   count(*) filter (where k = 'pv'), count(*) filter (where k = 'signup'), count(*) filter (where k = 'login')
	 from (
	   select project_id, visitor_hash, session_id, 'pv' as k from pageviews where ts >= $2 and ts < $3
	   union all
	   select project_id, visitor_hash, session_id, type from analytics_events where ts >= $2 and ts < $3 and type <> 'vital'
	 ) x group by project_id`,

	`delete from daily_page_stats where day = $1`,
	`insert into daily_page_stats (project_id, day, route, device, views, visitors, sessions, exits, load_ms_sum, load_ms_count)
	 with pv as (
	   select id, project_id, route, coalesce(device, 'unknown') as device, visitor_hash, session_id, ts, load_ms
	   from pageviews where ts >= $2 and ts < $3
	 ), last as (
	   select distinct on (project_id, session_id) project_id, session_id, route
	   from pv where session_id is not null order by project_id, session_id, ts desc, id desc -- a batch shares one ts: insertion order breaks the tie
	 )
	 select pv.project_id, $1::date, pv.route, pv.device, count(*), count(distinct pv.visitor_hash), count(distinct pv.session_id),
	   count(distinct pv.session_id) filter (where exists (
	     select 1 from last l where l.project_id = pv.project_id and l.session_id = pv.session_id and l.route = pv.route)),
	   coalesce(sum(pv.load_ms) filter (where pv.load_ms > 0), 0), count(*) filter (where pv.load_ms > 0)
	 from pv group by pv.project_id, pv.route, pv.device`,

	`delete from daily_source_stats where day = $1`,
	`insert into daily_source_stats (project_id, day, source, medium, campaign, sessions, visitors, signups)
	 with s as (
	   select project_id, session_id, visitor_hash,
	     coalesce(nullif(utm_source, ''), nullif(referrer_host, ''), 'direct') as source,
	     coalesce(utm_medium, '') as medium, coalesce(utm_campaign, '') as campaign
	   from analytics_events where type = 'session_start' and ts >= $2 and ts < $3
	 ), su as (
	   select distinct project_id, session_id from analytics_events where type = 'signup' and ts >= $2 and ts < $3
	 )
	 select s.project_id, $1::date, s.source, s.medium, s.campaign,
	   count(distinct s.session_id), count(distinct s.visitor_hash), count(distinct su.session_id)
	 from s left join su on su.project_id = s.project_id and su.session_id = s.session_id
	 group by s.project_id, s.source, s.medium, s.campaign`,

	`delete from daily_click_stats where day = $1`,
	`insert into daily_click_stats (project_id, day, route, label, clicks, visitors)
	 select project_id, $1::date, coalesce(route, ''), name, count(*), count(distinct visitor_hash)
	 from analytics_events where type = 'click' and coalesce(name, '') <> '' and ts >= $2 and ts < $3
	 group by project_id, coalesce(route, ''), name`,

	`delete from daily_event_stats where day = $1`,
	`insert into daily_event_stats (project_id, day, name, events, visitors)
	 select project_id, $1::date, name, count(*), count(distinct visitor_hash)
	 from analytics_events where type = 'custom' and ts >= $2 and ts < $3
	 group by project_id, name`,

	`delete from daily_dimension_stats where day = $1`,
	`insert into daily_dimension_stats (project_id, day, dimension, value, sessions, visitors)
	 select project_id, $1::date, 'device', coalesce(device, 'unknown'), count(distinct session_id), count(distinct visitor_hash)
	 from pageviews where ts >= $2 and ts < $3 group by project_id, coalesce(device, 'unknown')
	 union all
	 select project_id, $1::date, 'country', country, count(distinct session_id), count(distinct visitor_hash)
	 from pageviews where ts >= $2 and ts < $3 and coalesce(country, '') <> '' group by project_id, country`,

	// Every project gets the automatic sign-up goal.
	`insert into goals (project_id, name, kind, is_lead)
	 select p.id, 'Sign-ups', 'signup', true from projects p
	 where not exists (select 1 from goals g where g.project_id = p.id and g.kind = 'signup')`,

	`delete from daily_goal_stats where day = $1`,
	`insert into daily_goal_stats (project_id, day, goal_id, conversions, visitors)
	 with hits as (
	   select g.id as goal_id, g.project_id, a.session_id, a.visitor_hash
	   from goals g join analytics_events a on a.project_id = g.project_id and a.ts >= $2 and a.ts < $3
	   where (g.kind = 'signup' and a.type = 'signup')
	      or (g.kind = 'click' and a.type = 'click' and lower(a.name) = lower(g.match))
	      or (g.kind = 'event' and a.type = 'custom' and a.name = g.match)
	   union all
	   select g.id, g.project_id, p.session_id, p.visitor_hash
	   from goals g join pageviews p on p.project_id = g.project_id and p.ts >= $2 and p.ts < $3
	   where g.kind = 'page' and p.route = g.match
	 )
	 select project_id, $1::date, goal_id, count(distinct session_id), count(distinct visitor_hash)
	 from hits group by project_id, goal_id`,
}

// Step is one funnel step.
type Step struct {
	Kind  string `json:"kind"` // page, click, event, signup
	Match string `json:"match"`
}

// ParseSteps decodes a funnel's steps.
func ParseSteps(raw []byte) ([]Step, error) {
	var s []Step
	err := json.Unmarshal(raw, &s)
	return s, err
}

func (s Step) matches(kind, value string) bool {
	switch s.Kind {
	case "page":
		return kind == "page" && value == s.Match
	case "click":
		return kind == "click" && strings.EqualFold(value, s.Match)
	case "event":
		return kind == "event" && value == s.Match
	case "signup":
		return kind == "signup"
	}
	return false
}

// rollupFunnels walks each session's events in time order and counts how
// many sessions reached each step in sequence, per device.
func rollupFunnels(ctx context.Context, tx pgx.Tx, day, start, end time.Time) error {
	if _, err := tx.Exec(ctx, `delete from daily_funnel_stats where day = $1`, day); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select id, project_id, steps from funnels`)
	if err != nil {
		return err
	}
	type funnel struct {
		id, project int64
		steps       []Step
	}
	var funnels []funnel
	for rows.Next() {
		var f funnel
		var raw []byte
		if err := rows.Scan(&f.id, &f.project, &raw); err != nil {
			rows.Close()
			return err
		}
		if f.steps, err = ParseSteps(raw); err == nil && len(f.steps) > 0 {
			funnels = append(funnels, f)
		}
	}
	rows.Close()

	for _, f := range funnels {
		evs, err := tx.Query(ctx, `
			select session_id, coalesce(device, 'unknown'), kind, value from (
			  select session_id, device, 'page' as kind, route as value, ts, 0 as src, id from pageviews
			  where project_id = $1 and ts >= $2 and ts < $3 and session_id is not null
			  union all
			  select session_id, device, case type when 'custom' then 'event' else type end, coalesce(name, ''), ts, 1, id
			  from analytics_events
			  where project_id = $1 and ts >= $2 and ts < $3 and session_id is not null and type in ('click', 'custom', 'signup')
			) e order by session_id, ts, src, id`, f.project, start, end)
		if err != nil {
			return err
		}
		counts := map[string][]int{} // device -> sessions reaching step i
		var cur, dev string
		reached := 0
		finish := func() {
			if cur == "" {
				return
			}
			c := counts[dev]
			if c == nil {
				c = make([]int, len(f.steps))
				counts[dev] = c
			}
			for i := 0; i < reached; i++ {
				c[i]++
			}
		}
		for evs.Next() {
			var sid, device, kind, value string
			if err := evs.Scan(&sid, &device, &kind, &value); err != nil {
				evs.Close()
				return err
			}
			if sid != cur {
				finish()
				cur, dev, reached = sid, device, 0
			}
			if reached < len(f.steps) && f.steps[reached].matches(kind, value) {
				reached++
			}
		}
		evs.Close()
		finish()
		for device, c := range counts {
			for i, n := range c {
				if _, err := tx.Exec(ctx, `insert into daily_funnel_stats (project_id, day, funnel_id, step, device, sessions)
					values ($1, $2, $3, $4, $5, $6)`, f.project, day, f.id, i, device, n); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// RollupDay rebuilds every daily table for one UTC day.
func RollupDay(ctx context.Context, pool *pgxpool.Pool, day time.Time) error {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for i, s := range dayStatements {
			args := []any{start}
			if strings.Contains(s, "$2") {
				args = append(args, start, end)
			}
			if strings.HasPrefix(strings.TrimSpace(s), "insert into goals") {
				args = nil
			}
			if _, err := tx.Exec(ctx, s, args...); err != nil {
				return fmt.Errorf("rollup statement %d: %w", i, err)
			}
		}
		return rollupFunnels(ctx, tx, start, start, end)
	})
}

// RollupRetention updates weekly retention for identified accounts. Raw
// events only go back 30 days, so counts are merged with greatest(): weeks
// computed earlier keep their value after the raw data is gone.
func RollupRetention(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		with acct as (
		  select project_id, account_hash, date_trunc('week', ts)::date as week
		  from analytics_events where account_hash is not null
		  group by project_id, account_hash, date_trunc('week', ts)
		), cohort as (
		  select project_id, account_hash, min(week) as cohort_week from acct group by project_id, account_hash
		)
		insert into weekly_retention (project_id, cohort_week, week_offset, accounts)
		select a.project_id, c.cohort_week, ((a.week - c.cohort_week) / 7)::int, count(distinct a.account_hash)
		from acct a join cohort c using (project_id, account_hash)
		group by a.project_id, c.cohort_week, ((a.week - c.cohort_week) / 7)::int
		on conflict (project_id, cohort_week, week_offset)
		do update set accounts = greatest(weekly_retention.accounts, excluded.accounts)`)
	return err
}

// Run is the rollup_hourly job: today, yesterday and retention.
func Run(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	today := now.UTC()
	for _, d := range []time.Time{today.AddDate(0, 0, -1), today} {
		if err := RollupDay(ctx, pool, d); err != nil {
			return err
		}
	}
	return RollupRetention(ctx, pool)
}

// Backfill rebuilds the last n days, so a new funnel or goal shows history
// right away (raw data goes back 30 days).
func Backfill(ctx context.Context, pool *pgxpool.Pool, now time.Time, days int) error {
	for i := days - 1; i >= 0; i-- {
		if err := RollupDay(ctx, pool, now.UTC().AddDate(0, 0, -i)); err != nil {
			return err
		}
	}
	return nil
}
