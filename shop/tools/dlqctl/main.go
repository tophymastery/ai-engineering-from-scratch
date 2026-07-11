// Command dlqctl is the S-T6 DLQ operator CLI (implements the park/inspect/
// replay tooling of D22). It reads a consumer group's dead-letter queue and
// replays parked events back onto the backbone via the outbox, so reprocessing
// converges exactly-once through the consumer inbox.
//
// Usage:
//
//	dlqctl -db <sqlite-file> list    [-group G] [-status parked|replayed]
//	dlqctl -db <sqlite-file> inspect <id>
//	dlqctl -db <sqlite-file> replay  <id>            # one row
//	dlqctl -db <sqlite-file> replay  -group G -all   # every parked row in a group
//	dlqctl -db <sqlite-file> depth   [-group G]
//	dlqctl -db <sqlite-file> seed                    # demo: migrate + park samples
//
// -db defaults to the DLQ_DB env var. In production dlqctl points at the cell's
// consumer database (PG); here it drives a SQLite file so the CLI is runnable
// end-to-end without a server. Replay re-inserts into the outbox in that same
// database; the running relay (Debezium in prod, CDCTailRelay here) then
// republishes it. See RUNBOOK.md.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/shop-platform/shop/libs/eventbus"
	"github.com/shop-platform/shop/libs/inbox"
	"github.com/shop-platform/shop/libs/outbox"
	_ "modernc.org/sqlite"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(args []string, out io.Writer) int {
	// Global flags (may lead the command); flag.Parse stops at the subcommand.
	gfs := flag.NewFlagSet("dlqctl", flag.ContinueOnError)
	gfs.SetOutput(out)
	dbPath := gfs.String("db", os.Getenv("DLQ_DB"), "sqlite database file (or $DLQ_DB)")
	if err := gfs.Parse(args); err != nil {
		return 2
	}
	rest := gfs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(out, "usage: dlqctl -db <file> {list|inspect <id>|replay <id>|depth|seed}")
		return 2
	}
	// Subcommand-scoped flags, parsed from the args AFTER the subcommand so
	// `replay -group G -all` works.
	cmd := rest[0]
	sfs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	sfs.SetOutput(out)
	group := sfs.String("group", "", "consumer group filter")
	status := sfs.String("status", "", "status filter: parked|replayed")
	all := sfs.Bool("all", false, "replay: act on every parked row in -group")
	if err := sfs.Parse(rest[1:]); err != nil {
		return 2
	}
	rest = append([]string{cmd}, sfs.Args()...)
	if *dbPath == "" {
		fmt.Fprintln(out, "error: -db (or $DLQ_DB) is required")
		return 2
	}
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintf(out, "error: open db: %v\n", err)
		return 2
	}
	defer db.Close()
	dlq := inbox.NewSQLDLQ(db, inbox.SQLiteDialect{})

	switch rest[0] {
	case "seed":
		return cmdSeed(ctx, out, db, dlq)
	case "list":
		return cmdList(ctx, out, dlq, *group, *status)
	case "depth":
		return cmdDepth(ctx, out, dlq, *group)
	case "inspect":
		if len(rest) < 2 {
			fmt.Fprintln(out, "usage: dlqctl -db <file> inspect <id>")
			return 2
		}
		return cmdInspect(ctx, out, dlq, rest[1])
	case "replay":
		return cmdReplay(ctx, out, db, dlq, *group, *all, rest[1:])
	default:
		fmt.Fprintf(out, "unknown command %q\n", rest[0])
		return 2
	}
}

func cmdList(ctx context.Context, out io.Writer, dlq *inbox.SQLDLQ, group, status string) int {
	rows, err := dlq.List(ctx, group, status)
	if err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "%-5s %-20s %-14s %-14s %-8s %-9s %s\n", "ID", "EVENT_ID", "GROUP", "TOPIC", "ATTEMPTS", "STATUS", "PARKED_AT")
	for _, r := range rows {
		fmt.Fprintf(out, "%-5d %-20s %-14s %-14s %-8d %-9s %s\n",
			r.ID, r.EventID, r.Group, r.Topic, r.Attempts, r.Status, r.ParkedAt.Format("2006-01-02T15:04:05Z"))
	}
	fmt.Fprintf(out, "(%d rows)\n", len(rows))
	return 0
}

func cmdDepth(ctx context.Context, out io.Writer, dlq *inbox.SQLDLQ, group string) int {
	d, err := dlq.Depth(ctx, group)
	if err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "%d\n", d)
	return 0
}

func cmdInspect(ctx context.Context, out io.Writer, dlq *inbox.SQLDLQ, idStr string) int {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		fmt.Fprintf(out, "error: bad id %q\n", idStr)
		return 2
	}
	r, err := dlq.Get(ctx, id)
	if err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "id:         %d\n", r.ID)
	fmt.Fprintf(out, "event_id:   %s\n", r.EventID)
	fmt.Fprintf(out, "group:      %s\n", r.Group)
	fmt.Fprintf(out, "topic:      %s\n", r.Topic)
	fmt.Fprintf(out, "agg_key:    %s\n", r.Key)
	fmt.Fprintf(out, "attempts:   %d\n", r.Attempts)
	fmt.Fprintf(out, "status:     %s\n", r.Status)
	fmt.Fprintf(out, "cause:      %s\n", r.Cause)
	fmt.Fprintf(out, "parked_at:  %s\n", r.ParkedAt.Format("2006-01-02T15:04:05Z"))
	var pretty json.RawMessage = r.Payload
	if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
		fmt.Fprintf(out, "envelope:\n%s\n", b)
	}
	return 0
}

func cmdReplay(ctx context.Context, out io.Writer, db *sql.DB, dlq *inbox.SQLDLQ, group string, all bool, ids []string) int {
	store := outbox.NewSQLStore(db, outbox.SQLiteDialect{})
	republish := func(ctx context.Context, r inbox.ParkedRow) error {
		env, err := eventbus.UnmarshalEnvelope(r.Payload)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := store.WriteInTx(ctx, tx, r.Topic, env); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}

	var targets []int64
	if all {
		rows, err := dlq.List(ctx, group, inbox.StatusParked)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return 2
		}
		for _, r := range rows {
			targets = append(targets, r.ID)
		}
	} else {
		if len(ids) < 1 {
			fmt.Fprintln(out, "usage: dlqctl -db <file> replay <id>  (or replay -group G -all)")
			return 2
		}
		id, err := strconv.ParseInt(ids[0], 10, 64)
		if err != nil {
			fmt.Fprintf(out, "error: bad id %q\n", ids[0])
			return 2
		}
		targets = append(targets, id)
	}

	n := 0
	for _, id := range targets {
		if err := dlq.Replay(ctx, id, republish); err != nil {
			fmt.Fprintf(out, "error: replay id=%d: %v\n", id, err)
			return 1
		}
		fmt.Fprintf(out, "replayed id=%d -> re-emitted via outbox (relay will republish; inbox dedupes)\n", id)
		n++
	}
	fmt.Fprintf(out, "replayed %d row(s)\n", n)
	return 0
}

// cmdSeed makes the demo self-contained: migrate the schema and park two sample
// poison events so `list`/`inspect`/`replay` have something to act on.
func cmdSeed(ctx context.Context, out io.Writer, db *sql.DB, dlq *inbox.SQLDLQ) int {
	if err := outbox.Migrate(ctx, db, outbox.SQLiteDialect{}); err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return 2
	}
	if err := inbox.Migrate(ctx, db, inbox.SQLiteDialect{}); err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return 2
	}
	for i := 0; i < 2; i++ {
		env, _ := eventbus.NewEnvelope(fmt.Sprintf("evt_seed_%d", i), "order.paid", "trace",
			eventbus.Aggregate{Type: "order", ID: fmt.Sprintf("ord_seed_%d", i), Region: "bkk"}, 3,
			map[string]any{"order_id": fmt.Sprintf("ord_seed_%d", i), "amount": 100 + i}, mustTime())
		m, _ := eventbus.NewMessage("order.paid", env)
		if err := dlq.Park(ctx, m, "projection", 3, fmt.Errorf("seed: simulated permanent handler failure")); err != nil {
			fmt.Fprintf(out, "error: park: %v\n", err)
			return 2
		}
	}
	fmt.Fprintln(out, "seeded 2 parked events in group 'projection'")
	return 0
}
