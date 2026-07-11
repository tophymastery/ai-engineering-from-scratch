package index

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Engine is the in-process inverted index + shard router: the faithful sandbox
// model of a country's per-cell OpenSearch index (D17). Documents are partitioned
// into NumShards primary shards by H3-res-5 routing (geo.go); a geo query fans
// out only to the shards its radius covers (≤2, geo_test.go). Each shard has its
// own lock, so a bulk reindex on the ingest workers contends with feed queries
// only on the 1/NumShards of shards it is currently touching — that per-shard
// isolation plus a bounded ingest queue is the "bulk-index backpressure on
// dedicated ingest nodes … never contends with feed reads" property (D17),
// measured in perf_test.go.
type Engine struct {
	clock     Clock
	shards    []*shard
	debouncer *ratingDebouncer

	// dedicated ingest "nodes": a fixed worker pool draining a BOUNDED queue.
	// The bound is the backpressure — a 150k-item bulk update cannot run
	// unboundedly ahead of the workers and monopolise CPU/locks; producers block
	// once the queue is full.
	ingestCh   chan MerchantDoc
	ingestWG   sync.WaitGroup
	ingestOnce sync.Once
	ingestRL   *rateLimiter // bulk-index backpressure (rate cap); nil = unlimited

	freshMu  sync.Mutex
	freshLag []time.Duration // event→queryable lag samples (freshness)

	// Partial-update buffers: a store.status_changed or rating.updated can arrive
	// BEFORE the menu.updated that first places the merchant geographically (only
	// per-salt/per-topic ordering is guaranteed, D11). Those updates are stashed
	// here (LWW by version) and drained the moment the merchant is first indexed —
	// the faithful "upsert a partial document" behaviour OpenSearch gives.
	pendMu     sync.Mutex
	pendStatus map[string]pendingStatus
	pendRating map[string]pendingRating

	// locate maps merchant_id -> its shard index, so a store-status / rating
	// update (which carries no location) routes to its shard in O(1) instead of
	// scanning all shards. Merchants do not relocate in this model (a relocation
	// would be a delete + reindex); location comes from menu.updated and is stable.
	locateMu sync.RWMutex
	locate   map[string]int
}

type pendingStatus struct {
	open    bool
	version int64
	at      time.Time
}

type pendingRating struct {
	rating  float64
	count   int64
	version int64
	at      time.Time
}

// shard is one primary shard modelled on OpenSearch's immutable-segment reads:
// queries are LOCK-FREE. Documents are bucketed by res-5 cell; each cell bucket
// holds an ATOMIC snapshot slice, and the shard's cell→bucket map is itself an
// atomic snapshot. A reader loads the map snapshot and each bucket snapshot with
// plain atomic loads — it never takes a lock, so a bulk reindex hammering the
// ingest workers cannot inflate feed-query latency (D17 "bulk-index … never
// contends with feed reads"; proven in perf_test.go, feed p99 within ±10% during
// a 150k reindex). Writers serialize on wmu (per shard) and publish via
// copy-on-write, so a reader always sees a consistent, fully-formed snapshot.
type shard struct {
	wmu   sync.Mutex               // serializes WRITERS on this shard only
	state atomic.Pointer[shardState]
	byID  map[string]*doc // write-side index (guarded by wmu)
}

// shardState is an immutable snapshot of a shard's cell→bucket map. Adding a new
// cell copy-on-writes this map (rare, once a cell is first populated); updating an
// existing cell only atomic-stores that cell's bucket (common, cheap).
type shardState struct {
	cells map[Cell]*docBucket
}

// docBucket holds one cell's documents as an immutable atomic snapshot slice.
type docBucket struct {
	snap atomic.Pointer[[]*doc]
}

func (b *docBucket) load() []*doc {
	if p := b.snap.Load(); p != nil {
		return *p
	}
	return nil
}

func newShard() *shard {
	sh := &shard{byID: map[string]*doc{}}
	sh.state.Store(&shardState{cells: map[Cell]*docBucket{}})
	return sh
}

// put inserts/replaces a doc in its cell bucket via copy-on-write. Caller holds
// wmu.
func (sh *shard) put(d *doc) {
	sh.byID[d.merchantID] = d
	st := sh.state.Load()
	b := st.cells[d.cell]
	if b == nil {
		b = &docBucket{}
		next := make(map[Cell]*docBucket, len(st.cells)+1)
		for k, v := range st.cells {
			next[k] = v
		}
		next[d.cell] = b
		sh.state.Store(&shardState{cells: next})
	}
	cur := b.load()
	repl := make([]*doc, 0, len(cur)+1)
	for _, x := range cur {
		if x.merchantID != d.merchantID {
			repl = append(repl, x)
		}
	}
	repl = append(repl, d)
	b.snap.Store(&repl)
}

// removeFromCell drops a merchant from a cell bucket via copy-on-write. Caller
// holds wmu.
func (sh *shard) removeFromCell(merchantID string, cell Cell) {
	st := sh.state.Load()
	b := st.cells[cell]
	if b == nil {
		return
	}
	cur := b.load()
	repl := make([]*doc, 0, len(cur))
	for _, x := range cur {
		if x.merchantID != merchantID {
			repl = append(repl, x)
		}
	}
	b.snap.Store(&repl)
}

type doc struct {
	merchantID    string
	name          string
	lat, lng      float64
	cell          Cell
	shard         int
	items         []Item
	rating        float64
	ratingCount   int64
	open          bool
	menuVersion   int64
	statusVersion int64
	ratingVersion int64
	tokens        map[string]struct{}
	indexedAt     time.Time
}

// MerchantDoc is a search document at the ingest boundary (built from
// menu.updated + optional location/name, or supplied directly by the ingest API).
type MerchantDoc struct {
	MerchantID  string    `json:"merchant_id"`
	Name        string    `json:"name"`
	Lat         float64   `json:"lat"`
	Lng         float64   `json:"lng"`
	Items       []Item    `json:"items"`
	Open        bool      `json:"open"`
	Rating      float64   `json:"rating"`
	RatingCount int64     `json:"rating_count"`
	MenuVersion int64     `json:"menu_version"`
	EventAt     time.Time `json:"-"` // source-event time, for freshness lag
}

// Item is one menu item in a search document (02 §1 Money: integer minor units).
type Item struct {
	ItemID    string `json:"item_id"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Available bool   `json:"available"`
}

// Query is a geo(+text) search. RadiusM defaults to DefaultRadiusM.
type Query struct {
	Lat     float64
	Lng     float64
	Text    string
	RadiusM float64
	Limit   int
	OpenB   bool // when true, only OPEN stores (browse feed uses this)
}

// Hit is one search result.
type Hit struct {
	StoreID   string `json:"store_id"`
	Name      string `json:"name"`
	Rating    float64 `json:"rating"`
	DistanceM int    `json:"distance_m"`
	Open      bool   `json:"open"`
	Items     []Item `json:"items,omitempty"`
}

// DefaultRadiusM is the default feed/search radius (~5 km delivery radius).
const DefaultRadiusM = 5000.0

// EngineOptions configures the ingest pipeline.
type EngineOptions struct {
	IngestWorkers   int     // dedicated ingest "nodes" (default 4)
	IngestQueue     int     // bounded ingest queue depth = backpressure (default 512)
	IngestRatePerSec float64 // bulk-index rate cap (backpressure); 0 = DefaultIngestRate
	Clock           Clock
}

// DefaultIngestRate is the bulk-index backpressure rate cap (docs/sec across all
// ingest workers). D17's "bulk-index pipeline with backpressure on dedicated
// ingest nodes so a 150k-item chain-menu update never contends with feed reads":
// the cap keeps the reindex from saturating CPU, leaving query capacity free so
// feed p99 stays flat. A synchronous IndexMerchant (the event-consumer path) is
// NOT rate-limited — only the BulkIndex worker path is.
const DefaultIngestRate = 10000.0

// rateLimiter is a token bucket pacing the ingest workers (aggregate rate cap).
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	tokens float64
	max    float64
	last   time.Time
	clock  Clock
}

func newRateLimiter(rate float64, clock Clock) *rateLimiter {
	if rate <= 0 {
		return nil
	}
	return &rateLimiter{rate: rate, tokens: rate, max: rate, last: clock.Now(), clock: clock}
}

// wait blocks until a token is available, pacing callers to the configured rate.
func (r *rateLimiter) wait() {
	if r == nil {
		return
	}
	for {
		r.mu.Lock()
		now := r.clock.Now()
		r.tokens += now.Sub(r.last).Seconds() * r.rate
		r.last = now
		if r.tokens > r.max {
			r.tokens = r.max
		}
		if r.tokens >= 1 {
			r.tokens--
			r.mu.Unlock()
			return
		}
		deficit := (1 - r.tokens) / r.rate
		r.mu.Unlock()
		time.Sleep(time.Duration(deficit * float64(time.Second)))
	}
}

// NewEngine builds an empty index with NumShards shards and starts the ingest
// workers.
func NewEngine(opt EngineOptions) *Engine {
	if opt.Clock == nil {
		opt.Clock = SystemClock{}
	}
	if opt.IngestWorkers <= 0 {
		opt.IngestWorkers = 4
	}
	if opt.IngestQueue <= 0 {
		opt.IngestQueue = 512
	}
	if opt.IngestRatePerSec == 0 {
		opt.IngestRatePerSec = DefaultIngestRate
	}
	e := &Engine{
		clock:      opt.Clock,
		shards:     make([]*shard, NumShards),
		debouncer:  newRatingDebouncer(opt.Clock, DebounceWindow),
		pendStatus: map[string]pendingStatus{},
		pendRating: map[string]pendingRating{},
		locate:     map[string]int{},
	}
	for i := range e.shards {
		e.shards[i] = newShard()
	}
	if opt.IngestRatePerSec > 0 {
		e.ingestRL = newRateLimiter(opt.IngestRatePerSec, opt.Clock)
	}
	e.ingestCh = make(chan MerchantDoc, opt.IngestQueue)
	for i := 0; i < opt.IngestWorkers; i++ {
		e.ingestWG.Add(1)
		go func() {
			defer e.ingestWG.Done()
			for m := range e.ingestCh {
				e.ingestRL.wait() // bulk-index backpressure
				e.applyDoc(m)
			}
		}()
	}
	return e
}

// Close drains and stops the ingest workers.
func (e *Engine) Close() {
	e.ingestOnce.Do(func() { close(e.ingestCh) })
	e.ingestWG.Wait()
}

// IndexMerchant applies a full merchant document synchronously (the consumer's
// menu.updated path and tests). LWW: an out-of-order (lower menuVersion) update
// is ignored.
func (e *Engine) IndexMerchant(m MerchantDoc) { e.applyDoc(m) }

// BulkIndex enqueues docs onto the bounded ingest queue for the dedicated ingest
// workers. It BLOCKS when the queue is full — that block is the backpressure that
// keeps a 150k-item reindex from starving feed queries. Returns after every doc
// is enqueued (not necessarily applied); call DrainIngest to await application.
func (e *Engine) BulkIndex(docs []MerchantDoc) {
	for _, m := range docs {
		e.ingestCh <- m
	}
}

// DrainIngest blocks until the ingest queue is empty (all enqueued docs applied).
// Used by tests to bound a reindex's "queryable" point.
func (e *Engine) DrainIngest() {
	for len(e.ingestCh) > 0 {
		time.Sleep(200 * time.Microsecond)
	}
}

// applyDoc upserts a merchant document into its shard's cell bucket under
// copy-on-write. The version check + swap happen atomically under the shard write
// lock, so concurrent same-merchant menu updates arriving across salted
// partitions resolve to the highest version (last-write-wins), not to whichever
// goroutine committed last.
func (e *Engine) applyDoc(m MerchantDoc) {
	cell := LatLngToCell(m.Lat, m.Lng)
	sIdx := CellToShard(cell)
	sh := e.shards[sIdx]

	sh.wmu.Lock()
	prev := sh.byID[m.MerchantID]
	if prev != nil && m.MenuVersion > 0 && m.MenuVersion < prev.menuVersion {
		sh.wmu.Unlock()
		return // stale menu update — LWW
	}
	nd := &doc{
		merchantID:  m.MerchantID,
		name:        m.Name,
		lat:         m.Lat,
		lng:         m.Lng,
		cell:        cell,
		shard:       sIdx,
		items:       m.Items,
		open:        m.Open,
		rating:      m.Rating,
		ratingCount: m.RatingCount,
		menuVersion: m.MenuVersion,
		indexedAt:   e.clock.Now(),
	}
	if prev != nil {
		if m.Name == "" {
			nd.name = prev.name
		}
		// Preserve independently-projected fields (status/rating) unless this menu
		// update carries them.
		nd.open = prev.open
		nd.statusVersion = prev.statusVersion
		nd.rating = prev.rating
		nd.ratingCount = prev.ratingCount
		nd.ratingVersion = prev.ratingVersion
		if m.Rating != 0 || m.RatingCount != 0 {
			nd.rating = m.Rating
			nd.ratingCount = m.RatingCount
		}
		if prev.cell != cell {
			sh.removeFromCell(m.MerchantID, prev.cell) // moved cell within shard
		}
	}
	nd.tokens = tokenize(nd.name, nd.items)
	sh.put(nd)
	firstIndex := prev == nil
	sh.wmu.Unlock()

	e.locateMu.Lock()
	e.locate[m.MerchantID] = sIdx
	e.locateMu.Unlock()

	if !m.EventAt.IsZero() {
		e.recordFreshness(nd.indexedAt.Sub(m.EventAt))
	}
	if firstIndex {
		e.drainPending(m.MerchantID)
	}
}

// shardOf returns the shard a known merchant lives on, or (-1,false) if it is
// not yet indexed.
func (e *Engine) shardOf(merchantID string) (int, bool) {
	e.locateMu.RLock()
	idx, ok := e.locate[merchantID]
	e.locateMu.RUnlock()
	return idx, ok
}

// drainPending applies any store-status / rating updates that arrived before the
// merchant was first indexed (partial-upsert convergence).
func (e *Engine) drainPending(merchantID string) {
	e.pendMu.Lock()
	ps, hasStatus := e.pendStatus[merchantID]
	pr, hasRating := e.pendRating[merchantID]
	delete(e.pendStatus, merchantID)
	delete(e.pendRating, merchantID)
	e.pendMu.Unlock()
	if hasStatus {
		e.SetStoreStatus(merchantID, ps.open, ps.version, ps.at)
	}
	if hasRating {
		e.ApplyRating(merchantID, pr.rating, pr.count, pr.version, pr.at)
	}
}

// SetStoreStatus applies a store.status_changed projection (open/closed) with LWW
// by statusVersion. Returns false if the merchant is unknown or the update is
// stale.
func (e *Engine) SetStoreStatus(merchantID string, open bool, version int64, eventAt time.Time) bool {
	sIdx, known := e.shardOf(merchantID)
	if known {
		sh := e.shards[sIdx]
		sh.wmu.Lock()
		if prev, ok := sh.byID[merchantID]; ok {
			if version > 0 && version < prev.statusVersion {
				sh.wmu.Unlock()
				return false
			}
			nd := *prev
			nd.open = open
			nd.statusVersion = version
			nd.indexedAt = e.clock.Now()
			sh.put(&nd)
			sh.wmu.Unlock()
			if !eventAt.IsZero() {
				e.recordFreshness(nd.indexedAt.Sub(eventAt))
			}
			return true
		}
		sh.wmu.Unlock()
	}
	// Merchant not yet indexed — stash the status for when menu.updated arrives.
	e.pendMu.Lock()
	if cur, ok := e.pendStatus[merchantID]; !ok || version >= cur.version {
		e.pendStatus[merchantID] = pendingStatus{open: open, version: version, at: eventAt}
	}
	e.pendMu.Unlock()
	return false
}

// ApplyRating projects a rating.updated event through the D17 debouncer: at most
// one index write per merchant per DebounceWindow. Returns true if this event
// actually wrote the index (i.e. it was NOT debounced away).
func (e *Engine) ApplyRating(merchantID string, rating float64, count, version int64, eventAt time.Time) bool {
	agg, write := e.debouncer.Offer(merchantID, ratingAgg{Rating: rating, Count: count, Version: version})
	if !write {
		return false
	}
	e.writeRating(merchantID, agg, eventAt)
	return true
}

// FlushRatings writes any debounced ratings whose window has elapsed (the
// background ticker / test drives this after advancing the clock). Returns the
// number of index writes performed.
func (e *Engine) FlushRatings(eventAt time.Time) int {
	pending := e.debouncer.Flush()
	for mid, agg := range pending {
		e.writeRating(mid, agg, eventAt)
	}
	return len(pending)
}

func (e *Engine) writeRating(merchantID string, agg ratingAgg, eventAt time.Time) {
	if sIdx, known := e.shardOf(merchantID); known {
		sh := e.shards[sIdx]
		sh.wmu.Lock()
		if prev, ok := sh.byID[merchantID]; ok {
			nd := *prev
			nd.rating = agg.Rating
			nd.ratingCount = agg.Count
			nd.ratingVersion = agg.Version
			nd.indexedAt = e.clock.Now()
			sh.put(&nd)
			sh.wmu.Unlock()
			if !eventAt.IsZero() {
				e.recordFreshness(nd.indexedAt.Sub(eventAt))
			}
			return
		}
		sh.wmu.Unlock()
	}
	// Merchant not yet indexed — stash the rating for menu.updated arrival.
	e.pendMu.Lock()
	if cur, ok := e.pendRating[merchantID]; !ok || agg.Version >= cur.version {
		e.pendRating[merchantID] = pendingRating{rating: agg.Rating, count: agg.Count, version: agg.Version, at: eventAt}
	}
	e.pendMu.Unlock()
}

// Search runs a geo(+text) query: route to the covering shards (≤2), scan their
// documents within the radius, filter by text/open, rank by (rating desc, then
// distance asc), and return the top Limit. This is the read path browse + geo
// search share.
func (e *Engine) Search(q Query) []Hit {
	if q.RadiusM <= 0 {
		q.RadiusM = DefaultRadiusM
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	text := strings.ToLower(strings.TrimSpace(q.Text))
	var terms []string
	if text != "" {
		terms = strings.Fields(text)
	}

	// Route to the covering cells, grouped by shard, and scan ONLY those cells'
	// buckets — so query cost tracks docs-near-the-point, not the whole index.
	cells := CellsForQuery(q.Lat, q.Lng, q.RadiusM)
	byShard := map[int][]Cell{}
	for _, c := range cells {
		s := CellToShard(c)
		byShard[s] = append(byShard[s], c)
	}
	var hits []Hit
	for sIdx, cs := range byShard {
		st := e.shards[sIdx].state.Load() // LOCK-FREE read snapshot
		for _, c := range cs {
			b := st.cells[c]
			if b == nil {
				continue
			}
			for _, d := range b.load() {
				if q.OpenB && !d.open {
					continue
				}
				dist := haversineM(q.Lat, q.Lng, d.lat, d.lng)
				if dist > q.RadiusM {
					continue
				}
				if len(terms) > 0 && !matchesAll(d.tokens, terms) {
					continue
				}
				hits = append(hits, Hit{
					StoreID:   d.merchantID,
					Name:      d.name,
					Rating:    d.rating,
					DistanceM: int(dist),
					Open:      d.open,
					Items:     d.items,
				})
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Rating != hits[j].Rating {
			return hits[i].Rating > hits[j].Rating
		}
		return hits[i].DistanceM < hits[j].DistanceM
	})
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits
}

// DocCount returns the total number of indexed merchants (audit/stats).
func (e *Engine) DocCount() int {
	n := 0
	for _, sh := range e.shards {
		sh.wmu.Lock()
		n += len(sh.byID)
		sh.wmu.Unlock()
	}
	return n
}

// ShardHistogram returns the per-shard document counts (audit; salt/geo balance).
func (e *Engine) ShardHistogram() []int {
	h := make([]int, NumShards)
	for i, sh := range e.shards {
		sh.wmu.Lock()
		h[i] = len(sh.byID)
		sh.wmu.Unlock()
	}
	return h
}

func (e *Engine) recordFreshness(lag time.Duration) {
	if lag < 0 {
		lag = 0
	}
	e.freshMu.Lock()
	e.freshLag = append(e.freshLag, lag)
	e.freshMu.Unlock()
}

// FreshnessP99 returns the p99 of the recorded event→queryable lags.
func (e *Engine) FreshnessP99() time.Duration {
	e.freshMu.Lock()
	defer e.freshMu.Unlock()
	return percentileDur(e.freshLag, 99)
}

// FreshnessSamples returns how many freshness lags were recorded.
func (e *Engine) FreshnessSamples() int {
	e.freshMu.Lock()
	defer e.freshMu.Unlock()
	return len(e.freshLag)
}

func tokenize(name string, items []Item) map[string]struct{} {
	toks := map[string]struct{}{}
	for _, w := range strings.Fields(strings.ToLower(name)) {
		toks[w] = struct{}{}
	}
	for _, it := range items {
		for _, w := range strings.Fields(strings.ToLower(it.Name)) {
			toks[w] = struct{}{}
		}
	}
	return toks
}

func matchesAll(tokens map[string]struct{}, terms []string) bool {
	for _, t := range terms {
		if _, ok := tokens[t]; !ok {
			return false
		}
	}
	return true
}

func percentileDur(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(samples))
	copy(cp, samples)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(p / 100 * float64(len(cp)-1))
	return cp[idx]
}
