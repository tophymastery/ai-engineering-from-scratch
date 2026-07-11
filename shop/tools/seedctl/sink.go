package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Sink is where seedctl writes seeded aggregates. It is deliberately an
// interface so that TODAY it targets the _placeholder /kv public API, and a real
// slice service (order, catalog, driver) plugs in LATER with zero changes to the
// builder: each service exposes its own public create endpoint and gets its own
// Sink implementation. seedctl NEVER writes to a DB directly (03 §3 — seeds go
// through the same public code paths as production).
type Sink interface {
	// Put creates one aggregate. collection is the entity kind (e.g. "orders");
	// id is its ULID; body is its canonical JSON. Implementations must be
	// idempotent on (collection,id).
	Put(collection, id string, body []byte) error
	Name() string
}

// NullSink discards writes — used for --dump-only determinism checks and when no
// target stack is provided.
type NullSink struct{}

func (NullSink) Put(string, string, []byte) error { return nil }
func (NullSink) Name() string                      { return "null (dump-only)" }

// KVSink targets the S-T3 _placeholder /kv public API — the current stand-in
// sink. It POSTs {key,value} with a deterministic Idempotency-Key (02 §3), so
// re-running a seed converges to the same rows instead of duplicating them.
//
// ADAPTATION: /kv is a generic KV echo store, not a domain service. Each seeded
// aggregate is stored under key "<collection>/<id>" with its canonical JSON as
// the value. When the order/catalog/driver slices land, swap this for per-entity
// Sinks hitting POST /v1/orders, POST /v1/merchants, … — the builder is unchanged.
type KVSink struct {
	base   string
	client *http.Client
}

// NewKVSink builds a sink against a target base URL (e.g. http://localhost:8081).
func NewKVSink(base string) *KVSink {
	return &KVSink{base: base, client: &http.Client{Timeout: 5 * time.Second}}
}

func (k *KVSink) Name() string { return k.base + " (/kv public API)" }

func (k *KVSink) Put(collection, id string, body []byte) error {
	payload, _ := json.Marshal(map[string]string{
		"key":   collection + "/" + id,
		"value": string(body),
	})
	req, err := http.NewRequest(http.MethodPost, k.base+"/kv", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Deterministic idempotency key: same seed+scenario => same key => the /kv
	// idempotency middleware replays instead of double-writing.
	req.Header.Set("Idempotency-Key", "seed_"+collection+"_"+id)
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kv put %s/%s: status %d: %s", collection, id, resp.StatusCode, b)
	}
	return nil
}

// push writes every entity of a dataset to a Sink in canonical order and returns
// the number written.
func push(sink Sink, ds *Dataset) (int, error) {
	n := 0
	put := func(coll, id string, v any) error {
		b, _ := json.Marshal(v)
		if err := sink.Put(coll, id, b); err != nil {
			return err
		}
		n++
		return nil
	}
	for _, u := range ds.Users {
		if err := put("users", u.ID, u); err != nil {
			return n, err
		}
	}
	for _, m := range ds.Merchants {
		if err := put("merchants", m.ID, m); err != nil {
			return n, err
		}
	}
	for _, mi := range ds.MenuItems {
		if err := put("menu_items", mi.ID, mi); err != nil {
			return n, err
		}
	}
	for _, c := range ds.Carts {
		if err := put("carts", c.ID, c); err != nil {
			return n, err
		}
	}
	for _, d := range ds.Drivers {
		if err := put("drivers", d.ID, d); err != nil {
			return n, err
		}
	}
	for _, o := range ds.Orders {
		if err := put("orders", o.ID, o); err != nil {
			return n, err
		}
	}
	return n, nil
}
