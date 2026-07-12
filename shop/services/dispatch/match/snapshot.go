package match

import (
	"encoding/json"
	"math/rand"
	"time"
)

// snapshot.go — the deterministic logged snapshots that make dispatch replayable
// and explainable (D13: "Each batch logs its full input snapshot ⇒ deterministic
// and explainable (preserves 01 §6)"). Correctness property #1 (the headline):
// replaying a logged snapshot reproduces byte-identical assignments 100%.
//
// A Snapshot captures EVERYTHING the matcher consumed for one zone tick — the
// waiting orders, the available drivers, the RNG seed, and the tick time — plus
// the assignments it produced. Replay() re-runs the SAME matcher over the SAME
// inputs with a fresh rand seeded from the SAME seed and, because the ETAFunc is
// pure and every input is sorted, yields the identical assignment set. The log is
// queryable (GET /v1/admin/snapshots) so any assignment is explainable after the
// fact.

// Snapshot is one zone tick's full input+output record.
type Snapshot struct {
	TickID       int64        `json:"tick_id"`
	Zone         Zone         `json:"zone"`
	ZoneKey      string       `json:"zone_key"`
	Partition    int          `json:"partition"`
	At           time.Time    `json:"at"`
	Seed         int64        `json:"seed"`
	Orders       []Order      `json:"orders"`
	Drivers      []Driver     `json:"drivers"`
	Assignments  []Assignment `json:"assignments"`
}

// Replay re-runs the matcher over the snapshot's logged inputs, seeded from the
// snapshot's seed. Because Match sorts its inputs, reads ETAs only from the
// injected pure ETAFunc, and derives all randomness from the seed, the returned
// assignments are byte-identical to the ones taken live — that is the 100%
// deterministic-replay property. `eta` must be the same pure ETA source used live
// (the map-sim twin, or map-sim memoised into the snapshot).
func (s Snapshot) Replay(eta ETAFunc) []Assignment {
	rng := rand.New(rand.NewSource(s.Seed))
	return Match(s.Orders, s.Drivers, eta, rng)
}

// Canonical renders the snapshot's OUTPUT (its sorted assignments) to stable JSON
// bytes — the comparison unit for the "byte-identical" replay assertion.
func Canonical(as []Assignment) string {
	b, _ := json.Marshal(sortAssignments(append([]Assignment(nil), as...)))
	return string(b)
}

// ReplayMatches reports whether replaying the snapshot reproduces its logged
// assignments byte-for-byte.
func (s Snapshot) ReplayMatches(eta ETAFunc) bool {
	return Canonical(s.Replay(eta)) == Canonical(s.Assignments)
}
