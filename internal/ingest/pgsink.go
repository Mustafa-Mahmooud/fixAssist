package ingest

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/yourname/fixassist/internal/jobs/jobargs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store/db"
)

// PGSink writes events and their group_event jobs in one transaction, so an
// event is never stored without its job or the other way round.
type PGSink struct {
	Pool  *pgxpool.Pool
	River *river.Client[pgx.Tx]
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func (s *PGSink) Envelope(ctx context.Context, rec Received) error {
	if len(rec.Rows) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		params := make([]river.InsertManyParams, 0, len(rec.Rows))
		for _, r := range rec.Rows {
			ts := pgtype.Timestamptz{Time: r.ReceivedAt, Valid: true}
			id, err := q.InsertEvent(ctx, db.InsertEventParams{
				ProjectID: rec.Project.ID, ReceivedAt: ts, Kind: r.Kind, Fingerprint: r.Fingerprint,
				ReleaseID: text(r.ReleaseID), Route: text(r.Route), UserHash: text(r.UserHash), Payload: r.Payload,
			})
			if err != nil {
				return err
			}
			params = append(params, river.InsertManyParams{Args: jobargs.GroupEvent{EventID: id, ReceivedAt: r.ReceivedAt}})
		}
		month := projects.MonthStart(rec.Rows[0].ReceivedAt)
		if err := q.BumpUsage(ctx, db.BumpUsageParams{
			ProjectID: rec.Project.ID, Month: pgtype.Date{Time: month, Valid: true}, Events: int64(len(rec.Rows)),
		}); err != nil {
			return err
		}
		_, err := s.River.InsertManyTx(ctx, tx, params)
		return err
	})
}

func (s *PGSink) Pageview(ctx context.Context, rec Received) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		for _, pv := range rec.Pageviews {
			load := pgtype.Int4{}
			if pv.LoadMS != nil {
				load = pgtype.Int4{Int32: *pv.LoadMS, Valid: true}
			}
			if err := q.InsertPageview(ctx, db.InsertPageviewParams{
				ProjectID: rec.Project.ID, Ts: pgtype.Timestamptz{Time: pv.TS, Valid: true}, Route: pv.Route,
				SessionID: text(pv.SessionID), ReleaseID: text(pv.ReleaseID), LoadMs: load,
				VisitorHash: text(pv.VisitorHash), Country: text(pv.Country), Device: text(pv.Device),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Analytics stores usage events (one transaction per batch).
func (s *PGSink) Analytics(ctx context.Context, rec Received) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		for _, a := range rec.Analytics {
			if err := q.InsertAnalyticsEvent(ctx, db.InsertAnalyticsEventParams{
				ProjectID: rec.Project.ID, Ts: pgtype.Timestamptz{Time: a.TS, Valid: true}, Type: a.Type,
				SessionID: text(a.SessionID), VisitorHash: text(a.VisitorHash), AccountHash: text(a.AccountHash),
				Route: text(a.Route), Name: text(a.Name), Props: a.Props, ReferrerHost: text(a.ReferrerHost),
				UtmSource: text(a.UTMSource), UtmMedium: text(a.UTMMedium), UtmCampaign: text(a.UTMCampaign),
				Country: text(a.Country), Device: text(a.Device),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
