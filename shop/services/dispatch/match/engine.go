package match

import (
	"sort"
	"sync"
	"time"
)

// engine.go — the in-memory zone-owned batch-matching engine that ties the
// matcher, the reservation ledger, and the snapshot log together. It is the pure
// domain core of the dispatch service: the main package wraps it with HTTP, the
// eventbus/inbox, and a SQLite snapshot store, but ALL of the D13 correctness
// lives here so it is exercised directly under -race without a network.
//
// Zone ownership (D13, correctness property #2): every order/driver belongs to
// exactly one zone (its H3 res-5 cell). A tick operates on ONE zone; the engine
// serialises ticks per zone (a zone lock) so a zone has a single writer per tick —
// no two ticks assign the same driver. Different zones tick concurrently (their
// order/driver sets are disjoint), which the concurrency test drives under -race.

// Offer is a reserved (order, driver) pair awaiting the driver's accept. While the
// offer stands the driver is EXCLUSIVELY reserved (10 s TTL) — no other batch can
// offer that driver, so offers never race to a 409.
type Offer struct {
	OrderID   string    `json:"order_id"`
	DriverID  string    `json:"driver_id"`
	Zone      Zone      `json:"zone"`
	PickupETA int       `json:"pickup_eta_s"`
	OfferedAt time.Time `json:"offered_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AssignedResult is a consumed offer: the driver accepted and is now assigned.
type AssignedResult struct {
	OrderID    string    `json:"order_id"`
	DriverID   string    `json:"driver_id"`
	PickupETA  int       `json:"pickup_eta_s"`
	AssignedAt time.Time `json:"assigned_at"`
}

// zoneState is one zone's mutable set: its waiting orders and available drivers.
type zoneState struct {
	mu      sync.Mutex // the single-writer-per-zone lock
	orders  map[string]Order
	drivers map[string]Driver
}

func newZoneState() *zoneState {
	return &zoneState{orders: map[string]Order{}, drivers: map[string]Driver{}}
}

// Engine is the zone-owned batch matcher.
type Engine struct {
	clock    Clock
	eta      ETAFunc
	ledger   *Ledger
	baseSeed int64
	partN    int // configured Kafka partition count (topology render-only; pinning real)

	mu     sync.Mutex
	zones  map[string]*zoneState
	offers map[string]*Offer         // by order_id
	byDrv  map[string]string         // driver_id → order_id (its live offer)
	assign map[string]AssignedResult // by order_id
	snaps  []Snapshot
	tickID int64
}

// Config configures an Engine.
type Config struct {
	Clock      Clock
	ETA        ETAFunc // defaults to the map-sim twin ETASeconds
	TTL        time.Duration
	BaseSeed   int64
	Partitions int
}

// NewEngine builds a zone-owned batch-matching engine.
func NewEngine(cfg Config) *Engine {
	eta := cfg.ETA
	if eta == nil {
		eta = ETASeconds
	}
	part := cfg.Partitions
	if part <= 0 {
		part = 64
	}
	return &Engine{
		clock:    cfg.Clock,
		eta:      eta,
		ledger:   NewLedger(cfg.TTL),
		baseSeed: cfg.BaseSeed,
		partN:    part,
		zones:    map[string]*zoneState{},
		offers:   map[string]*Offer{},
		byDrv:    map[string]string{},
		assign:   map[string]AssignedResult{},
	}
}

// Ledger exposes the reservation ledger (stats/sweep).
func (e *Engine) Ledger() *Ledger { return e.ledger }

func (e *Engine) zoneOf(z Zone) *zoneState {
	e.mu.Lock()
	defer e.mu.Unlock()
	zs, ok := e.zones[z.Key()]
	if !ok {
		zs = newZoneState()
		e.zones[z.Key()] = zs
	}
	return zs
}

// AddOrder registers a waiting order in its zone (derived from its pickup point).
func (e *Engine) AddOrder(o Order) Zone {
	z := ZoneFor(o.Pickup)
	zs := e.zoneOf(z)
	zs.mu.Lock()
	zs.orders[o.OrderID] = o
	zs.mu.Unlock()
	return z
}

// AddDriver registers an available driver in its zone (derived from its location).
func (e *Engine) AddDriver(d Driver) Zone {
	z := ZoneFor(d.Loc)
	zs := e.zoneOf(z)
	zs.mu.Lock()
	zs.drivers[d.DriverID] = d
	zs.mu.Unlock()
	return z
}

// Tick runs one batch-match for a single zone: it snapshots the zone's waiting
// orders + offerable drivers, runs greedy-with-swaps under a per-tick seed,
// reserves each matched driver EXCLUSIVELY, emits offers, removes matched orders
// from the waiting set, and logs the snapshot. Returns the offers made. Holding
// the zone lock for the whole tick is the single-writer-per-zone guarantee.
func (e *Engine) Tick(z Zone) []Offer {
	zs := e.zoneOf(z)
	zs.mu.Lock()
	defer zs.mu.Unlock()

	now := e.clock.Now()

	// Snapshot inputs: waiting orders + drivers NOT already reserved/offered/
	// assigned. Sorted deterministically by the matcher.
	orders := make([]Order, 0, len(zs.orders))
	for _, o := range zs.orders {
		orders = append(orders, o)
	}
	drivers := make([]Driver, 0, len(zs.drivers))
	for _, d := range zs.drivers {
		if e.driverBusy(d.DriverID, now) {
			continue
		}
		drivers = append(drivers, d)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].OrderID < orders[j].OrderID })
	sort.Slice(drivers, func(i, j int) bool { return drivers[i].DriverID < drivers[j].DriverID })

	// Per-tick seed: deterministic from the base seed + the monotonic tick id, so
	// the live run and any replay share the seed ⇒ identical assignments.
	e.mu.Lock()
	tickID := e.tickID
	e.tickID++
	e.mu.Unlock()
	seed := e.baseSeed + tickID

	as := matchWithSeed(orders, drivers, e.eta, seed)

	snap := Snapshot{
		TickID: tickID, Zone: z, ZoneKey: z.Key(), Partition: z.Partition(e.partN),
		At: now, Seed: seed, Orders: orders, Drivers: drivers, Assignments: as,
	}

	made := make([]Offer, 0, len(as))
	for _, a := range as {
		// Reserve the driver EXCLUSIVELY before offering. With zone ownership this
		// always succeeds (the driver was filtered as offerable); a conflict here is
		// counted and the order simply stays waiting for the next tick — never a 409.
		if !e.ledger.Reserve(a.DriverID, a.OrderID, z, now) {
			continue
		}
		of := &Offer{
			OrderID: a.OrderID, DriverID: a.DriverID, Zone: z, PickupETA: a.PickupETA,
			OfferedAt: now, ExpiresAt: now.Add(e.ledger.ttl),
		}
		e.mu.Lock()
		e.offers[a.OrderID] = of
		e.byDrv[a.DriverID] = a.OrderID
		e.mu.Unlock()
		delete(zs.orders, a.OrderID) // matched — leave the waiting set
		made = append(made, *of)
	}

	e.mu.Lock()
	e.snaps = append(e.snaps, snap)
	e.mu.Unlock()
	return made
}

// driverBusy reports whether a driver already has a live reservation, a standing
// offer, or an assignment — i.e. is not offerable this tick.
func (e *Engine) driverBusy(driverID string, now time.Time) bool {
	if e.ledger.IsHeld(driverID, now) {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, offered := e.byDrv[driverID]
	return offered
}

// TickAll ticks every zone that currently has a waiting order and returns all
// offers made. Zone order is stable (sorted keys) for reproducibility.
func (e *Engine) TickAll() []Offer {
	e.mu.Lock()
	keys := make([]string, 0, len(e.zones))
	zmap := make(map[string]Zone, len(e.zones))
	for k, zs := range e.zones {
		zs.mu.Lock()
		n := len(zs.orders)
		if len(zs.orders) > 0 {
			// recover the Zone from any order's pickup
			for _, o := range zs.orders {
				zmap[k] = ZoneFor(o.Pickup)
				break
			}
		}
		zs.mu.Unlock()
		if n > 0 {
			keys = append(keys, k)
		}
	}
	e.mu.Unlock()
	sort.Strings(keys)
	var all []Offer
	for _, k := range keys {
		all = append(all, e.Tick(zmap[k])...)
	}
	return all
}

// Offer returns the standing offer for an order, if any.
func (e *Engine) Offer(orderID string) (Offer, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if of, ok := e.offers[orderID]; ok {
		return *of, true
	}
	return Offer{}, false
}

// OffersForDriver returns the standing offer for a driver, if any.
func (e *Engine) OffersForDriver(driverID string) (Offer, bool) {
	e.mu.Lock()
	orderID, ok := e.byDrv[driverID]
	if !ok {
		e.mu.Unlock()
		return Offer{}, false
	}
	of, ok := e.offers[orderID]
	e.mu.Unlock()
	if !ok {
		return Offer{}, false
	}
	return *of, true
}

// Accept consumes the offer for orderID: the driver accepts, its reservation is
// consumed, and the order becomes assigned. Idempotent — a second accept of an
// already-assigned order returns the same result. Returns ok=false if there is no
// live offer to accept (e.g. it expired first).
func (e *Engine) Accept(orderID string) (AssignedResult, bool) {
	now := e.clock.Now()
	e.mu.Lock()
	if a, ok := e.assign[orderID]; ok { // already assigned — idempotent
		e.mu.Unlock()
		return a, true
	}
	of, ok := e.offers[orderID]
	if !ok {
		e.mu.Unlock()
		return AssignedResult{}, false
	}
	driverID := of.DriverID
	zone := of.Zone
	pickupETA := of.PickupETA
	e.mu.Unlock()

	// Consume the exclusive reservation (the accept). If it fails the offer
	// expired — the order returns to waiting via the sweeper.
	if !e.ledger.Consume(driverID, orderID, now) {
		return AssignedResult{}, false
	}

	res := AssignedResult{OrderID: orderID, DriverID: driverID, PickupETA: pickupETA, AssignedAt: now}
	e.mu.Lock()
	e.assign[orderID] = res
	delete(e.offers, orderID)
	delete(e.byDrv, driverID)
	e.mu.Unlock()

	// The driver is now committed to this order — remove it from its zone's
	// available pool so no later tick offers it again.
	zs := e.zoneOf(zone)
	zs.mu.Lock()
	delete(zs.drivers, driverID)
	zs.mu.Unlock()
	return res, true
}

// Assignment returns the recorded assignment for an order, if it has one.
func (e *Engine) Assignment(orderID string) (AssignedResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.assign[orderID]
	return a, ok
}

// SweepExpired reclaims every expired reservation and returns each un-accepted
// order to its zone's waiting set (its driver is offerable again). Returns the
// number of offers reclaimed. This is the reservation-leak safety net — after it
// runs (at any time), no expired hold remains, so the ledger's Leaked() is 0.
func (e *Engine) SweepExpired(now time.Time) int {
	e.mu.Lock()
	expired := make([]*Offer, 0)
	for _, of := range e.offers {
		if !now.Before(of.ExpiresAt) {
			expired = append(expired, of)
		}
	}
	for _, of := range expired {
		delete(e.offers, of.OrderID)
		delete(e.byDrv, of.DriverID)
	}
	e.mu.Unlock()

	for _, of := range expired {
		e.ledger.Release(of.DriverID)  // account the reservation as released
		zs := e.zoneOf(of.Zone)        // return the order to waiting
		zs.mu.Lock()
		zs.orders[of.OrderID] = Order{OrderID: of.OrderID, Pickup: pickupOfZone(of)}
		zs.mu.Unlock()
	}
	// Also reap any ledger holds that expired without a tracked offer (belt & braces).
	e.ledger.Sweep(now)
	return len(expired)
}

// pickupOfZone reconstructs a representative pickup for a returned order. The
// order's precise pickup is re-supplied when it is re-added by the caller; this
// keeps the zone bucket consistent in the meantime (zone centroid).
func pickupOfZone(of *Offer) Point {
	return Point{Lat: float64(of.Zone.Lat)*ZoneDegLat + ZoneDegLat/2, Lng: float64(of.Zone.Lng)*ZoneDegLat + ZoneDegLat/2}
}

// Snapshots returns a copy of the snapshot log (queryable — GET /v1/admin/snapshots).
func (e *Engine) Snapshots() []Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Snapshot(nil), e.snaps...)
}

// SnapshotByTick returns one logged snapshot by tick id.
func (e *Engine) SnapshotByTick(id int64) (Snapshot, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.snaps {
		if s.TickID == id {
			return s, true
		}
	}
	return Snapshot{}, false
}

// ETA exposes the engine's ETA source (for replay verification with the same
// pure function the engine matched with).
func (e *Engine) ETA() ETAFunc { return e.eta }

// WaitingCount is the number of orders still waiting across all zones.
func (e *Engine) WaitingCount() int {
	e.mu.Lock()
	zs := make([]*zoneState, 0, len(e.zones))
	for _, z := range e.zones {
		zs = append(zs, z)
	}
	e.mu.Unlock()
	n := 0
	for _, z := range zs {
		z.mu.Lock()
		n += len(z.orders)
		z.mu.Unlock()
	}
	return n
}
