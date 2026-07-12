package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// rebuilddemo.go — the `merchant-queue -rebuild-demo` command (D7 Tier-1
// rebuild-from-events tooling; run once by `make rebuild-merchant-queue`). It
// seeds N orders' worth of order.* events through the real projection (building
// the read model + the append-only log), then REBUILDS the largest physical cell
// from the log and asserts the rebuilt model equals the live one (100% parity).
// It prints the wall time (the "rebuild of the largest cell < 1h" datum — real
// counts, wall-clock adapted to sandbox scale) and exits nonzero on any mismatch.

func runRebuildDemo(n int) {
	ctx := context.Background()
	st, err := openStore(ctx, "bkk")
	if err != nil {
		fmt.Fprintf(os.Stderr, "rebuild-demo: open store: %v\n", err)
		os.Exit(1)
	}
	defer st.close()
	pr := newProjection(st, SystemClock{})

	const merchants = 200
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fmt.Printf("rebuild-demo: seeding %d orders across %d merchants (order.created→paid→…)\n", n, merchants)
	seedStart := time.Now()
	for i := 0; i < n; i++ {
		orderID := fmt.Sprintf("ord_demo_%08d", i)
		merchantID := fmt.Sprintf("mer_demo_%04d", i%merchants)
		at := base.Add(time.Duration(i) * time.Second)
		inject(ctx, pr, "evt_c_"+orderID, TopicOrderCreated, orderID, "", at, map[string]any{"customer_id": "usr_demo", "total": map[string]any{"amount": 42550, "currency": "THB"}, "status": "PAYMENT_PENDING"})
		inject(ctx, pr, "evt_p_"+orderID, TopicOrderPaid, orderID, merchantID, at.Add(3*time.Second), map[string]any{"total": map[string]any{"amount": 42550, "currency": "THB"}, "paid_at": at.Format(time.RFC3339)})
		switch i % 5 {
		case 0, 1, 2:
			inject(ctx, pr, "evt_a_"+orderID, TopicOrderAccepted, orderID, merchantID, at.Add(10*time.Second), map[string]any{"accepted_at": at.Format(time.RFC3339)})
		case 3:
			inject(ctx, pr, "evt_x_"+orderID, TopicOrderCancelled, orderID, merchantID, at.Add(10*time.Second), map[string]any{"cancelled_at": at.Format(time.RFC3339)})
		}
	}
	orders, _ := st.orderCount(ctx)
	logN, _ := st.logCount(ctx)
	fmt.Printf("rebuild-demo: seeded %d orders, %d log events in %v\n", orders, logN, time.Since(seedStart))

	// Find the largest physical cell (D11) and rebuild ONLY it.
	counts, _ := st.cellCounts(ctx)
	largest, largestN := -1, -1
	for cell, c := range counts {
		if c > largestN {
			largest, largestN = cell, c
		}
	}
	fmt.Printf("rebuild-demo: cell row counts %v; largest = cell %d with %d orders\n", counts, largest, largestN)

	cellRes, err := st.Rebuild(ctx, largest, SystemClock{}.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "rebuild-demo: rebuild cell %d: %v\n", largest, err)
		os.Exit(1)
	}
	fmt.Printf("rebuild-demo: LARGEST-CELL rebuild — cell=%d orders=%d events=%d duration=%v parity_ok=%v mismatches=%d\n",
		cellRes.Cell, cellRes.Orders, cellRes.Events, cellRes.Duration, cellRes.ParityOK, cellRes.Mismatches)

	// Whole-store rebuild for the full parity proof.
	allRes, err := st.Rebuild(ctx, -1, SystemClock{}.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "rebuild-demo: full rebuild: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("rebuild-demo: FULL rebuild — orders=%d events=%d duration=%v parity_ok=%v mismatches=%d\n",
		allRes.Orders, allRes.Events, allRes.Duration, allRes.ParityOK, allRes.Mismatches)

	if !cellRes.ParityOK || !allRes.ParityOK {
		fmt.Fprintln(os.Stderr, "rebuild-demo: PARITY FAILED")
		os.Exit(1)
	}
	fmt.Printf("rebuild-demo: OK — largest cell (%d orders) + full store rebuilt from the event log with 100%% parity\n", largestN)
}

func inject(ctx context.Context, pr *Projection, eventID, eventType, orderID, merchantID string, at time.Time, extra map[string]any) {
	env, err := makeOrderEnvelope(eventID, eventType, orderID, merchantID, "bkk", extra, at)
	if err != nil {
		panic(err)
	}
	if _, err := pr.InjectEnvelope(ctx, env); err != nil {
		panic(err)
	}
}
