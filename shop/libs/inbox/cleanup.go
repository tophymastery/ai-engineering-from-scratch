package inbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// InboxRetention is the D8 inbox retention: 7 days. Older partitions are
// dropped — a redelivery of a >7-day-old event is astronomically unlikely (the
// bus tiers at 7 d hot / 90 d, D8), and if one occurred the effect is naturally
// bounded by the business state machine.
const InboxRetention = 7 * 24 * time.Hour

// DropInboxBefore removes inbox partitions strictly older than cutoff. On PG
// this is DROP PARTITION (O(1), zero dead tuples); the portable form here is a
// DELETE by part_day. Returns rows removed.
func DropInboxBefore(ctx context.Context, db *sql.DB, d Dialect, cutoff time.Time) (int, error) {
	return dropByDay(ctx, db, d, "inbox", cutoff)
}

// DropInboxOlderThanRetention drops inbox partitions older than InboxRetention.
func DropInboxOlderThanRetention(ctx context.Context, db *sql.DB, d Dialect) (int, error) {
	return DropInboxBefore(ctx, db, d, time.Now().UTC().Add(-InboxRetention))
}

// DropReplayedDLQBefore drops DLQ rows that were already replayed and are older
// than cutoff (parked-but-unreplayed rows are retained for operator action).
func DropReplayedDLQBefore(ctx context.Context, db *sql.DB, d Dialect, cutoff time.Time) (int, error) {
	q := fmt.Sprintf(`DELETE FROM dlq WHERE status = %s AND part_day < %s`,
		d.Placeholder(1), d.Placeholder(2))
	res, err := db.ExecContext(ctx, q, StatusReplayed, cutoff.UTC().Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func dropByDay(ctx context.Context, db *sql.DB, d Dialect, table string, cutoff time.Time) (int, error) {
	q := fmt.Sprintf(`DELETE FROM %s WHERE part_day < %s`, table, d.Placeholder(1))
	res, err := db.ExecContext(ctx, q, cutoff.UTC().Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
