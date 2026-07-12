package match

import (
	"math/rand"
	"sort"
)

// matcher.go — the D13 batch matcher: a per-order greedy BASELINE and the shipped
// greedy-with-swaps matcher, both deterministic. D13's rationale is that per-order
// greedy "concentrates offers on the same top drivers and degrades exactly when
// supply is scarce"; the batch matcher recovers the globally better assignment via
// local swaps. Correctness property #4 (VERIFICATION §V-T12): on a skewed dataset
// the batch matcher's sum-of-pickup-ETA is ≥10% lower than the greedy baseline.
//
// DETERMINISM (correctness property #1): every input is sorted by a stable key
// before matching, ETAs come from the injected pure ETAFunc, and the only source
// of nondeterminism — the random restarts of the local search — is driven by a
// SEEDED *rand.Rand. Same inputs + same seed ⇒ byte-identical assignment, which is
// what makes snapshot replay reproduce assignments 100%.

// Order is a waiting order in a zone: its pickup point is the merchant location.
type Order struct {
	OrderID string `json:"order_id"`
	Pickup  Point  `json:"pickup"`
}

// Driver is an available driver in a zone.
type Driver struct {
	DriverID string `json:"driver_id"`
	Loc      Point  `json:"loc"`
}

// Assignment is one matched (order, driver) pair with its pickup ETA in seconds.
type Assignment struct {
	OrderID   string `json:"order_id"`
	DriverID  string `json:"driver_id"`
	PickupETA int    `json:"pickup_eta_s"`
}

// ETAFunc returns the pickup ETA in seconds for a driver at `from` collecting an
// order at `to`. Pure + deterministic (the map-sim twin, or a real map-sim call
// memoised into a snapshot). The matcher never reads anything else.
type ETAFunc func(from, to Point) int

// swapRestarts is the number of seeded local-search restarts the batch matcher
// runs. The first start is the greedy solution; the rest are seeded shuffles.
// Keeping the best (ties broken deterministically) reliably reaches the optimal
// assignment on the small per-zone batches D13 targets. Bounded so a tick stays
// within its 1–2 s budget.
const swapRestarts = 6

// matchWithSeed is Match with the rng constructed from an explicit seed — the
// exact call the engine makes per tick and that Snapshot.Replay reproduces. Same
// (orders, drivers, eta, seed) ⇒ identical assignments.
func matchWithSeed(orders []Order, drivers []Driver, eta ETAFunc, seed int64) []Assignment {
	return Match(orders, drivers, eta, rand.New(rand.NewSource(seed)))
}

// GreedyBaseline is the naive per-order matcher D13 improves on: orders in stable
// id order each grab their nearest still-free driver. Deterministic (id-sorted,
// ETA ties broken by driver id). This is the baseline the batch matcher must beat
// by ≥10% on the skewed dataset.
func GreedyBaseline(orders []Order, drivers []Driver, eta ETAFunc) []Assignment {
	os := sortedOrders(orders)
	ds := sortedDrivers(drivers)
	used := make([]bool, len(ds))
	out := make([]Assignment, 0, len(os))
	for _, o := range os {
		best, bestETA := -1, 0
		for j, d := range ds {
			if used[j] {
				continue
			}
			e := eta(d.Loc, o.Pickup)
			if best == -1 || e < bestETA {
				best, bestETA = j, e
			}
		}
		if best == -1 {
			continue // no free driver — order stays waiting
		}
		used[best] = true
		out = append(out, Assignment{OrderID: o.OrderID, DriverID: ds[best].DriverID, PickupETA: bestETA})
	}
	return sortAssignments(out)
}

// Match is the shipped greedy-with-swaps batch matcher. It starts from the greedy
// assignment then runs a 2-opt + relocation local search to convergence, across
// `swapRestarts` seeded restarts, keeping the lowest total pickup ETA. The rand is
// seeded and its call sequence is fixed by the (sorted) inputs, so the result is
// deterministic for a given seed — the property snapshot replay depends on.
func Match(orders []Order, drivers []Driver, eta ETAFunc, rng *rand.Rand) []Assignment {
	os := sortedOrders(orders)
	ds := sortedDrivers(drivers)
	if len(os) == 0 || len(ds) == 0 {
		return nil
	}
	// Pre-compute the full cost matrix once (cost[i][j] = ETA of driver j → order
	// i). Pure ETAFunc ⇒ identical matrix on replay.
	cost := make([][]int, len(os))
	for i := range os {
		cost[i] = make([]int, len(ds))
		for j := range ds {
			cost[i][j] = eta(ds[j].Loc, os[i].Pickup)
		}
	}
	m := min(len(os), len(ds))

	// assignment: assign[i] = driver index for order i, or -1. The first start is
	// the greedy order (order 0..m-1 each take their nearest free driver); later
	// starts shuffle the order-processing sequence with the seeded rng.
	var best []int
	var bestTotal int
	for r := 0; r < swapRestarts; r++ {
		seq := make([]int, len(os))
		for i := range seq {
			seq[i] = i
		}
		if r > 0 {
			rng.Shuffle(len(seq), func(a, b int) { seq[a], seq[b] = seq[b], seq[a] })
		}
		assign := greedySeed(seq, cost, len(ds), m)
		localSearch(assign, cost)
		tot := total(assign, cost)
		if best == nil || tot < bestTotal {
			best = append([]int(nil), assign...)
			bestTotal = tot
		}
	}

	out := make([]Assignment, 0, m)
	for i, j := range best {
		if j < 0 {
			continue
		}
		out = append(out, Assignment{OrderID: os[i].OrderID, DriverID: ds[j].DriverID, PickupETA: cost[i][j]})
	}
	return sortAssignments(out)
}

// greedySeed builds an initial assignment by processing orders in `seq` order,
// each taking its nearest still-free driver (ETA ties broken by lower driver
// index for determinism). At most m orders get a driver.
func greedySeed(seq []int, cost [][]int, nDrivers, m int) []int {
	assign := make([]int, len(cost))
	for i := range assign {
		assign[i] = -1
	}
	used := make([]bool, nDrivers)
	placed := 0
	for _, i := range seq {
		if placed >= m {
			break
		}
		best, bestETA := -1, 0
		for j := 0; j < nDrivers; j++ {
			if used[j] {
				continue
			}
			if best == -1 || cost[i][j] < bestETA {
				best, bestETA = j, cost[i][j]
			}
		}
		if best >= 0 {
			assign[i] = best
			used[best] = true
			placed++
		}
	}
	return assign
}

// localSearch improves an assignment in place via 2-opt swaps (exchange the
// drivers of two assigned orders) and relocations (move an assigned order onto a
// currently-unused driver), iterating to convergence. Both move families only
// ever accept a strictly-lower total, so the search terminates. Deterministic:
// candidate pairs are scanned in index order, so no tie-break randomness leaks in.
func localSearch(assign []int, cost [][]int) {
	n := len(assign)
	nDrivers := 0
	if n > 0 {
		nDrivers = len(cost[0])
	}
	for {
		improved := false
		// 2-opt: swap drivers between two assigned orders.
		for i := 0; i < n; i++ {
			if assign[i] < 0 {
				continue
			}
			for k := i + 1; k < n; k++ {
				if assign[k] < 0 {
					continue
				}
				a, b := assign[i], assign[k]
				before := cost[i][a] + cost[k][b]
				after := cost[i][b] + cost[k][a]
				if after < before {
					assign[i], assign[k] = b, a
					improved = true
				}
			}
		}
		// relocation: move an assigned order onto a free driver if cheaper.
		used := make([]bool, nDrivers)
		for _, j := range assign {
			if j >= 0 {
				used[j] = true
			}
		}
		for i := 0; i < n; i++ {
			if assign[i] < 0 {
				continue
			}
			cur := assign[i]
			for j := 0; j < nDrivers; j++ {
				if used[j] || j == cur {
					continue
				}
				if cost[i][j] < cost[i][cur] {
					used[cur] = false
					used[j] = true
					assign[i] = j
					cur = j
					improved = true
				}
			}
		}
		if !improved {
			return
		}
	}
}

func total(assign []int, cost [][]int) int {
	t := 0
	for i, j := range assign {
		if j >= 0 {
			t += cost[i][j]
		}
	}
	return t
}

// TotalETA is the sum-of-pickup-ETA of an assignment set (the batch-quality
// metric compared against the greedy baseline).
func TotalETA(as []Assignment) int {
	t := 0
	for _, a := range as {
		t += a.PickupETA
	}
	return t
}

func sortedOrders(in []Order) []Order {
	out := append([]Order(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out
}

func sortedDrivers(in []Driver) []Driver {
	out := append([]Driver(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].DriverID < out[j].DriverID })
	return out
}

func sortAssignments(in []Assignment) []Assignment {
	sort.Slice(in, func(i, j int) bool { return in[i].OrderID < in[j].OrderID })
	return in
}
