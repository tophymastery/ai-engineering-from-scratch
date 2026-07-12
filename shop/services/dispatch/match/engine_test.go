package match

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

var engBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTestEngine(clk Clock) *Engine {
	return NewEngine(Config{Clock: clk, TTL: 10 * time.Second, BaseSeed: 1000, Partitions: 64})
}

// TestOfferAcceptFlow: the happy path — a waiting order + an available driver →
// tick reserves the driver and emits an offer → accept consumes the reservation
// and records the assignment. No 409 anywhere.
func TestOfferAcceptFlow(t *testing.T) {
	clk := NewManualClock(engBase)
	e := newTestEngine(clk)
	e.AddDriver(Driver{DriverID: "drv_1", Loc: Point{Lat: 13.75, Lng: 100.50}})
	z := e.AddOrder(Order{OrderID: "ord_1", Pickup: Point{Lat: 13.75, Lng: 100.51}})

	offers := e.Tick(z)
	if len(offers) != 1 || offers[0].DriverID != "drv_1" {
		t.Fatalf("expected one offer to drv_1, got %+v", offers)
	}
	if _, ok := e.Offer("ord_1"); !ok {
		t.Fatal("order should have a standing offer")
	}
	res, ok := e.Accept("ord_1")
	if !ok || res.DriverID != "drv_1" {
		t.Fatalf("accept failed: %+v ok=%v", res, ok)
	}
	// idempotent re-accept.
	if r2, ok := e.Accept("ord_1"); !ok || r2 != res {
		t.Fatalf("re-accept not idempotent: %+v", r2)
	}
	st := e.Ledger().Stats(clk.Now())
	if st.Consumed != 1 || st.Leaked != 0 {
		t.Fatalf("want consumed=1 leaked=0, got %+v", st)
	}
}

// TestSnapshotReplay100 is correctness property #1 (the headline): replaying every
// logged snapshot reproduces byte-identical assignments 100%. Drives many ticks
// across many zones with a realistic random population, then replays each logged
// snapshot and asserts identical assignments.
func TestSnapshotReplay100(t *testing.T) {
	clk := NewManualClock(engBase)
	e := newTestEngine(clk)
	rng := rand.New(rand.NewSource(2026))

	// Populate ~30 zones with skewed density (some zones hot, some sparse).
	for i := 0; i < 400; i++ {
		z := rng.Intn(30)
		lat := 13.0 + float64(z)*ZoneDegLat + rng.Float64()*0.05
		lng := 100.0 + rng.Float64()*0.05
		e.AddOrder(Order{OrderID: fmt.Sprintf("ord_%04d", i), Pickup: Point{Lat: lat, Lng: lng}})
		if i%2 == 0 {
			e.AddDriver(Driver{DriverID: fmt.Sprintf("drv_%04d", i), Loc: Point{Lat: lat + rng.Float64()*0.03, Lng: lng + rng.Float64()*0.03}})
		}
	}
	// Several rounds of ticks (some orders re-tick as drivers free up).
	for round := 0; round < 5; round++ {
		e.TickAll()
		clk.Advance(2 * time.Second)
	}

	snaps := e.Snapshots()
	if len(snaps) == 0 {
		t.Fatal("no snapshots logged")
	}
	replayed := 0
	for _, s := range snaps {
		if !s.ReplayMatches(ETASeconds) {
			t.Fatalf("snapshot tick %d (zone %s) did NOT replay identically:\n live=%s\nreplay=%s",
				s.TickID, s.ZoneKey, Canonical(s.Assignments), Canonical(s.Replay(ETASeconds)))
		}
		replayed++
	}
	t.Logf("deterministic snapshot replay: %d/%d snapshots reproduced identical assignments (100%%)", replayed, len(snaps))
}

// TestZoneSingleWriter is correctness property #2: no two ticks assign the same
// driver. Concurrent ticks on the SAME zone are serialised by the zone lock (the
// single-writer guarantee), and across the whole run every driver is offered to at
// most one order. Runs under -race.
func TestZoneSingleWriter(t *testing.T) {
	clk := NewManualClock(engBase)
	e := newTestEngine(clk)
	// One hot zone: 50 orders, 50 drivers, all in the same res-5 cell.
	baseLat, baseLng := 13.75, 100.5
	for i := 0; i < 50; i++ {
		e.AddOrder(Order{OrderID: fmt.Sprintf("ord_%03d", i), Pickup: Point{Lat: baseLat + float64(i)*0.0005, Lng: baseLng}})
		e.AddDriver(Driver{DriverID: fmt.Sprintf("drv_%03d", i), Loc: Point{Lat: baseLat + float64(i)*0.0005, Lng: baseLng + 0.001}})
	}
	z := ZoneFor(Point{Lat: baseLat, Lng: baseLng})

	var wg sync.WaitGroup
	seen := sync.Map{}
	var mu sync.Mutex
	var doubleOffer bool
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, of := range e.Tick(z) {
				if _, dup := seen.LoadOrStore(of.DriverID, of.OrderID); dup {
					mu.Lock()
					doubleOffer = true
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if doubleOffer {
		t.Fatal("a driver was offered to two orders — zone single-writer violated")
	}
	// Conflicts must be ~zero (exclusive reservations); definitely < 0.5%.
	if r := e.Ledger().ConflictRate(); r >= 0.005 {
		t.Fatalf("offer-conflict rate %.4f >= 0.5%%", r)
	}
	t.Logf("zone single-writer: 8 concurrent ticks on one hot zone, conflict rate=%.4f%%", e.Ledger().ConflictRate()*100)
}

// TestConcurrentBatchesNoConflictNoLeak is correctness property #3 at scale: many
// zones batch concurrently under -race; the exclusive reservations keep the
// offer-conflict rate < 0.5% with NO first-accept-wins 409, and after the 24 h
// soak sweep the reservation-leak rate is 0.
func TestConcurrentBatchesNoConflictNoLeak(t *testing.T) {
	clk := NewManualClock(engBase)
	e := newTestEngine(clk)
	rng := rand.New(rand.NewSource(7))

	const zones = 40
	type zoneInfo struct{ z Zone }
	var infos []zoneInfo
	for zi := 0; zi < zones; zi++ {
		lat0 := 5.0 + float64(zi)*ZoneDegLat*3 // separate zones
		var z Zone
		for i := 0; i < 30; i++ {
			lat := lat0 + rng.Float64()*0.05
			lng := 100.0 + rng.Float64()*0.05
			z = e.AddOrder(Order{OrderID: fmt.Sprintf("ord_%02d_%02d", zi, i), Pickup: Point{Lat: lat, Lng: lng}})
			if i%2 == 0 {
				e.AddDriver(Driver{DriverID: fmt.Sprintf("drv_%02d_%02d", zi, i), Loc: Point{Lat: lat, Lng: lng}})
			}
		}
		infos = append(infos, zoneInfo{z})
	}

	// Tick all zones concurrently, then accept a random subset of offers concurrently.
	var wg sync.WaitGroup
	offersCh := make(chan Offer, 4096)
	for _, in := range infos {
		wg.Add(1)
		go func(z Zone) {
			defer wg.Done()
			for _, of := range e.Tick(z) {
				offersCh <- of
			}
		}(in.z)
	}
	wg.Wait()
	close(offersCh)

	var accWg sync.WaitGroup
	acceptedDrv := sync.Map{}
	var mu sync.Mutex
	var doubleAssign int
	for of := range offersCh {
		if rng.Intn(100) < 60 { // 60% of offers accepted; 40% left to expire
			accWg.Add(1)
			go func(of Offer) {
				defer accWg.Done()
				if res, ok := e.Accept(of.OrderID); ok {
					if _, dup := acceptedDrv.LoadOrStore(res.DriverID, res.OrderID); dup {
						mu.Lock()
						doubleAssign++
						mu.Unlock()
					}
				}
			}(of)
		}
	}
	accWg.Wait()

	if doubleAssign != 0 {
		t.Fatalf("%d drivers were assigned to two orders (exclusivity broken)", doubleAssign)
	}
	conflict := e.Ledger().ConflictRate()
	if conflict >= 0.005 {
		t.Fatalf("offer-conflict rate %.4f%% >= 0.5%%", conflict*100)
	}

	// 24 h soak: advance the clock a full day and sweep. Every un-accepted
	// reservation must be reclaimed — leak rate 0.
	clk.Advance(24 * time.Hour)
	e.SweepExpired(clk.Now())
	st := e.Ledger().Stats(clk.Now())
	t.Logf("concurrent batches: conflict=%.4f%% leaked=%d ledger=%+v", conflict*100, st.Leaked, st)
	if st.Leaked != 0 {
		t.Fatalf("reservation leak rate must be 0 after 24h soak, leaked=%d", st.Leaked)
	}
	if st.Created != st.Consumed+st.Released {
		t.Fatalf("ledger accounting broken: created=%d consumed=%d released=%d", st.Created, st.Consumed, st.Released)
	}
}

// TestReservationSoak24h: a long simulated soak where offers are made every few
// seconds over 24 h and only some are accepted; advancing the clock (never
// sleeping) and sweeping keeps the leak rate identically 0 throughout.
func TestReservationSoak24h(t *testing.T) {
	clk := NewManualClock(engBase)
	e := newTestEngine(clk)
	rng := rand.New(rand.NewSource(11))
	drivers := 200
	for i := 0; i < drivers; i++ {
		e.AddDriver(Driver{DriverID: fmt.Sprintf("drv_%03d", i), Loc: Point{Lat: 13.75 + rng.Float64()*0.05, Lng: 100.5 + rng.Float64()*0.05}})
	}
	// 24h / 2s tick = 43200 ticks is a lot; sample every 30s for a fast but real soak.
	step := 30 * time.Second
	ticks := int((24 * time.Hour) / step)
	orderNo := 0
	for k := 0; k < ticks; k++ {
		// add a fresh order into a live zone
		orderNo++
		z := e.AddOrder(Order{OrderID: fmt.Sprintf("ord_%06d", orderNo), Pickup: Point{Lat: 13.75 + rng.Float64()*0.05, Lng: 100.5 + rng.Float64()*0.05}})
		for _, of := range e.Tick(z) {
			if rng.Intn(100) < 50 {
				e.Accept(of.OrderID)
			}
		}
		clk.Advance(step)
		e.SweepExpired(clk.Now()) // reclaim expired offers each tick
	}
	st := e.Ledger().Stats(clk.Now())
	t.Logf("24h soak (%d ticks): leaked=%d %+v", ticks, st.Leaked, st)
	if st.Leaked != 0 {
		t.Fatalf("reservation leak after 24h soak = %d, want 0", st.Leaked)
	}
}

// TestOfferExpiryReoffer: an offer that is never accepted expires; SweepExpired
// returns the order to waiting and frees the driver; a re-tick offers it again.
func TestOfferExpiryReoffer(t *testing.T) {
	clk := NewManualClock(engBase)
	e := newTestEngine(clk)
	e.AddDriver(Driver{DriverID: "drv_1", Loc: Point{Lat: 13.75, Lng: 100.50}})
	z := e.AddOrder(Order{OrderID: "ord_1", Pickup: Point{Lat: 13.75, Lng: 100.51}})
	if len(e.Tick(z)) != 1 {
		t.Fatal("expected an offer")
	}
	// let it expire without accepting
	clk.Advance(11 * time.Second)
	if n := e.SweepExpired(clk.Now()); n != 1 {
		t.Fatalf("expected 1 expired offer reclaimed, got %d", n)
	}
	// re-add the order's precise pickup and re-tick — same driver offered again.
	e.AddOrder(Order{OrderID: "ord_1", Pickup: Point{Lat: 13.75, Lng: 100.51}})
	offers := e.Tick(z)
	if len(offers) != 1 || offers[0].DriverID != "drv_1" {
		t.Fatalf("re-offer failed: %+v", offers)
	}
	st := e.Ledger().Stats(clk.Now())
	if st.Leaked != 0 {
		t.Fatalf("leak after expiry+reoffer = %d, want 0", st.Leaked)
	}
}
