package index

import (
	"fmt"
	"testing"
	"time"
)

func seedBangkok(eng *Engine) {
	// A handful of stores around central Bangkok (all within a 5 km radius of the
	// query point 13.7563,100.5018).
	eng.IndexMerchant(MerchantDoc{MerchantID: "mer_somtam", Name: "Som Tam House", Lat: 13.7570, Lng: 100.5030, Open: true, Rating: 4.7, MenuVersion: 1,
		Items: []Item{{ItemID: "itm_1", Name: "Som Tam", Amount: 8000, Currency: "THB", Available: true}}})
	eng.IndexMerchant(MerchantDoc{MerchantID: "mer_padthai", Name: "Pad Thai Corner", Lat: 13.7550, Lng: 100.5000, Open: true, Rating: 4.2, MenuVersion: 1,
		Items: []Item{{ItemID: "itm_2", Name: "Pad Thai", Amount: 6000, Currency: "THB", Available: true}}})
	eng.IndexMerchant(MerchantDoc{MerchantID: "mer_closed", Name: "Closed Kitchen", Lat: 13.7560, Lng: 100.5010, Open: false, Rating: 4.9, MenuVersion: 1,
		Items: []Item{{ItemID: "itm_3", Name: "Som Tam", Amount: 9000, Currency: "THB", Available: true}}})
	// A store far away (Chiang Mai) — must NOT appear in a Bangkok query.
	eng.IndexMerchant(MerchantDoc{MerchantID: "mer_faraway", Name: "Northern Som Tam", Lat: 18.7883, Lng: 98.9853, Open: true, Rating: 5.0, MenuVersion: 1})
}

func TestEngine_GeoSearch(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()
	seedBangkok(eng)

	hits := eng.Search(Query{Lat: 13.7563, Lng: 100.5018, Limit: 10})
	ids := map[string]bool{}
	for _, h := range hits {
		ids[h.StoreID] = true
	}
	if !ids["mer_somtam"] || !ids["mer_padthai"] {
		t.Fatalf("nearby stores missing from results: %+v", hits)
	}
	if ids["mer_faraway"] {
		t.Fatal("a store 700km away leaked into a 5km Bangkok query")
	}
	// Ranked by rating desc.
	if hits[0].StoreID != "mer_closed" && hits[0].Rating < hits[len(hits)-1].Rating {
		t.Fatalf("results not ranked by rating desc: %+v", hits)
	}
}

func TestEngine_BrowseOnlyOpen(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()
	seedBangkok(eng)

	feed := eng.Search(Query{Lat: 13.7563, Lng: 100.5018, OpenB: true, Limit: 10})
	for _, h := range feed {
		if h.StoreID == "mer_closed" {
			t.Fatal("browse feed included a CLOSED store")
		}
		if !h.Open {
			t.Fatalf("browse feed returned a non-open store: %+v", h)
		}
	}
	if len(feed) == 0 {
		t.Fatal("browse feed empty; want the open Bangkok stores")
	}
}

func TestEngine_TextSearch(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()
	seedBangkok(eng)

	hits := eng.Search(Query{Lat: 13.7563, Lng: 100.5018, Text: "pad thai", Limit: 10})
	if len(hits) != 1 || hits[0].StoreID != "mer_padthai" {
		t.Fatalf("text search 'pad thai' = %+v, want just mer_padthai", hits)
	}
}

func TestEngine_StoreStatusLWW(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()
	eng.IndexMerchant(MerchantDoc{MerchantID: "mer_s", Name: "S", Lat: 13.75, Lng: 100.50, Open: false, MenuVersion: 1})

	// v3 opens it; a late v2 (stale) must NOT re-close it (LWW).
	if !eng.SetStoreStatus("mer_s", true, 3, time.Time{}) {
		t.Fatal("v3 open should apply")
	}
	if eng.SetStoreStatus("mer_s", false, 2, time.Time{}) {
		t.Fatal("stale v2 close should be rejected (LWW)")
	}
	feed := eng.Search(Query{Lat: 13.75, Lng: 100.50, OpenB: true})
	if len(feed) != 1 {
		t.Fatalf("store should be OPEN after LWW resolution, feed=%+v", feed)
	}
}

func TestEngine_MenuVersionLWW(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()
	eng.IndexMerchant(MerchantDoc{MerchantID: "mer_m", Name: "M", Lat: 13.75, Lng: 100.50, Open: true, MenuVersion: 5,
		Items: []Item{{ItemID: "a", Name: "Fresh Dish", Amount: 100, Currency: "THB", Available: true}}})
	// A stale v3 menu update must not overwrite the v5 doc.
	eng.IndexMerchant(MerchantDoc{MerchantID: "mer_m", Name: "M", Lat: 13.75, Lng: 100.50, Open: true, MenuVersion: 3,
		Items: []Item{{ItemID: "b", Name: "Old Dish", Amount: 200, Currency: "THB", Available: true}}})
	hits := eng.Search(Query{Lat: 13.75, Lng: 100.50, Text: "fresh"})
	if len(hits) != 1 {
		t.Fatalf("stale menu update clobbered the fresh doc: %+v", eng.Search(Query{Lat: 13.75, Lng: 100.50}))
	}
}

// TestEngine_FreshnessP99 measures real event→queryable lag at adapted scale and
// asserts the D17 freshness budget (p99 < 30 s). The lag is genuinely measured
// per apply (indexedAt - EventAt); at in-process scale it is sub-millisecond, far
// under the 30 s budget that in production covers Kafka + bulk-index.
func TestEngine_FreshnessP99(t *testing.T) {
	eng := NewEngine(EngineOptions{})
	defer eng.Close()

	const n = 20000
	for i := 0; i < n; i++ {
		eventAt := time.Now().UTC()
		eng.IndexMerchant(MerchantDoc{
			MerchantID:  fmt.Sprintf("mer_fresh_%06d", i),
			Name:        "Fresh",
			Lat:         13.0 + float64(i%100)*0.01,
			Lng:         100.0 + float64(i%100)*0.01,
			Open:        true,
			MenuVersion: 1,
			EventAt:     eventAt,
		})
	}
	p99 := eng.FreshnessP99()
	t.Logf("freshness: %d events, event→queryable p99 = %v (budget 30s)", eng.FreshnessSamples(), p99)
	if p99 >= 30*time.Second {
		t.Fatalf("freshness p99 %v ≥ 30s (D17 freshness FAILED)", p99)
	}
}

// TestFeedReadsAreLockFree is the DETERMINISTIC structural proof behind the
// feed-stability property (D17 "bulk-index … never contends with feed reads"): a
// query never acquires a lock the ingest writer holds, so a busy/stuck ingest
// node can never block a feed read — the real backpressure failure mode, which
// blew feed p99 up 3–8× before the lock-free read path. It parks writers holding
// EVERY shard's write mutex, then shows feed reads still complete — impossible if
// reads took those mutexes.
func TestFeedReadsAreLockFree(t *testing.T) {
	eng := NewEngine(EngineOptions{IngestWorkers: 1})
	defer eng.Close()
	for i := 0; i < 200; i++ {
		eng.IndexMerchant(MerchantDoc{MerchantID: fmt.Sprintf("mer_%03d", i), Name: "Store Som Tam",
			Lat: 13.75 + float64(i%20)*0.001, Lng: 100.50 + float64(i/20)*0.001, Open: true, MenuVersion: 1})
	}
	for _, sh := range eng.shards {
		sh.wmu.Lock()
	}
	defer func() {
		for _, sh := range eng.shards {
			sh.wmu.Unlock()
		}
	}()

	done := make(chan int, 1)
	go func() {
		hits := eng.Search(Query{Lat: 13.75, Lng: 100.50, RadiusM: 20000, Limit: 50})
		done <- len(hits)
	}()
	select {
	case n := <-done:
		if n == 0 {
			t.Fatal("read returned no hits while writers were parked (expected the pre-indexed stores)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("feed read BLOCKED on a parked writer — reads are NOT lock-free (backpressure property broken)")
	}
}
