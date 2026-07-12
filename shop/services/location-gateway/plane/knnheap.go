package plane

import (
	"container/heap"
	"sort"
	"time"
)

// knnheap.go — a bounded size-k max-heap of nearest candidates for GeoStore.KNN.
// Keeping only the k best seen so far makes a query O(candidates · log k) instead
// of sorting every candidate in a dense cell (the p99 tail). The heap orders by
// (distance desc, driver_id desc) so heap[0] is the current WORST of the top-k —
// the exact k-th nearest — and offering a closer driver evicts it. The final
// result is re-sorted ascending (distance, then driver_id) for a deterministic,
// nearest-first kNN identical to a brute-force sort.

type nbrItem struct {
	pos  Position
	dist float64
}

type nbrHeap struct {
	lat, lng float64
	k        int
	items    []nbrItem
}

func (h *nbrHeap) Len() int { return len(h.items) }

// Less makes this a MAX-heap on distance (ties: larger driver_id is "greater", so
// the item evicted first on a tie is the lexically-larger id — matching the final
// ascending (dist, id) ordering).
func (h *nbrHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	if a.dist != b.dist {
		return a.dist > b.dist
	}
	return a.pos.DriverID > b.pos.DriverID
}

func (h *nbrHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *nbrHeap) Push(x any) { h.items = append(h.items, x.(nbrItem)) }

func (h *nbrHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	h.items = old[:n-1]
	return it
}

// offer considers a candidate position: if the heap is below k it is added; once
// full, it replaces the current worst iff it is nearer (or equal-distance with a
// smaller id, so the deterministic tie-break matches brute force).
func (h *nbrHeap) offer(p Position) {
	d := HaversineM(h.lat, h.lng, p.Lat, p.Lng)
	if len(h.items) < h.k {
		heap.Push(h, nbrItem{pos: p, dist: d})
		return
	}
	worst := h.items[0]
	if d < worst.dist || (d == worst.dist && p.DriverID < worst.pos.DriverID) {
		h.items[0] = nbrItem{pos: p, dist: d}
		heap.Fix(h, 0)
	}
}

func (h *nbrHeap) full() bool { return len(h.items) >= h.k }

// maxDist is the current k-th nearest distance (the heap root). Valid when full.
func (h *nbrHeap) maxDist() float64 {
	if len(h.items) == 0 {
		return 0
	}
	return h.items[0].dist
}

// sorted returns the heap contents as Neighbors, nearest first (distance then
// driver_id), i.e. the final kNN result.
func (h *nbrHeap) sorted() []Neighbor {
	out := make([]Neighbor, 0, len(h.items))
	for _, it := range h.items {
		out = append(out, Neighbor{
			DriverID:   it.pos.DriverID,
			Lat:        it.pos.Lat,
			Lng:        it.pos.Lng,
			DistanceM:  it.dist,
			H3Cell:     it.pos.Cell.Key(),
			RecordedAt: it.pos.RecordedAt.UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DistanceM != out[j].DistanceM {
			return out[i].DistanceM < out[j].DistanceM
		}
		return out[i].DriverID < out[j].DriverID
	})
	return out
}
