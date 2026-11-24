package store

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionedTables are split into daily UTC partitions named <table>_pYYYYMMDD.
var PartitionedTables = []string{"events", "pageviews", "analytics_events"}

// RetentionDays is how long raw events and page views are kept (FDR: 30 days).
const RetentionDays = 30

func partitionName(table string, day time.Time) string {
	return fmt.Sprintf("%s_p%s", table, day.Format("20060102"))
}

// EnsurePartitions creates daily partitions from now-back to now+ahead days.
// Safe to rerun.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, now time.Time, back, ahead int) ([]string, error) {
	today := now.UTC().Truncate(24 * time.Hour)
	var created []string
	for _, table := range PartitionedTables {
		for d := -back; d <= ahead; d++ {
			day := today.AddDate(0, 0, d)
			name := partitionName(table, day)
			sql := fmt.Sprintf(
				"create table if not exists %s partition of %s for values from ('%s') to ('%s')",
				pgx.Identifier{name}.Sanitize(), pgx.Identifier{table}.Sanitize(),
				day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339))
			if _, err := pool.Exec(ctx, sql); err != nil {
				return created, fmt.Errorf("%s: %w", name, err)
			}
			created = append(created, name)
		}
	}
	return created, nil
}

// DropOldPartitions drops daily partitions whose whole day is older than
// keepDays. Returns the dropped partition names.
func DropOldPartitions(ctx context.Context, pool *pgxpool.Pool, now time.Time, keepDays int) ([]string, error) {
	cutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -keepDays)
	var dropped []string
	for _, table := range PartitionedTables {
		rows, err := pool.Query(ctx, `
			select c.relname
			from pg_inherits i
			join pg_class c on c.oid = i.inhrelid
			join pg_class p on p.oid = i.inhparent
			where p.relname = $1`, table)
		if err != nil {
			return dropped, err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return dropped, err
		}
		prefix := table + "_p"
		for _, n := range names {
			if !strings.HasPrefix(n, prefix) {
				continue
			}
			day, err := time.Parse("20060102", strings.TrimPrefix(n, prefix))
			if err != nil || !day.Before(cutoff) {
				continue
			}
			if _, err := pool.Exec(ctx, "drop table "+pgx.Identifier{n}.Sanitize()); err != nil {
				return dropped, fmt.Errorf("drop %s: %w", n, err)
			}
			dropped = append(dropped, n)
		}
	}
	return dropped, nil
}

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
