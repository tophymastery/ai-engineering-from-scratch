package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shop-platform/shop/libs/logging"
)

func newTestServer(t *testing.T, jurs ...string) *server {
	t.Helper()
	if len(jurs) == 0 {
		jurs = []string{"ID", "VN", "SG"}
	}
	kr, err := newKeyring(randomKey(dekLen))
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStores(context.Background(), kr, jurs)
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	t.Cleanup(st.close)
	return &server{
		stores: st, kr: kr, ev: newEventBuilder("test"),
		log:        logging.New(logging.Config{Service: "identity-profile-test", Out: &bytes.Buffer{}}),
		defaultJur: jurs[0], homed: homedSet(jurs),
	}
}

func (s *server) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", s.handleHealth)
	m.HandleFunc("/v1/profiles", s.only(http.MethodPost, s.handleCreate))
	m.HandleFunc("/v1/profiles/", s.handleProfileSubtree)
	m.HandleFunc("/v1/tokens/", s.only(http.MethodGet, s.handleResolve))
	m.HandleFunc("/v1/orders:replay", s.only(http.MethodPost, s.handleReplay))
	return m
}

func do(t *testing.T, h http.Handler, method, path, cell, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if cell != "" {
		req.Header.Set("X-Cell", cell)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestProfileCRUD(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	body := `{"jurisdiction":"ID","full_name":"Budi Santoso","phone":"+62-812-1111-2222","email":"budi@example.co.id","addresses":[{"label":"home","line1":"Jl. Merdeka 17","city":"Jakarta","postal":"10110"}]}`
	code, out := do(t, h, "POST", "/v1/profiles", "ID", body)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	usr, _ := out["user_token"].(string)
	if tokenKind(usr) != "user" {
		t.Fatalf("bad user_token: %v", out)
	}
	addrs, _ := out["addresses"].([]any)
	if len(addrs) != 1 {
		t.Fatalf("expected 1 address: %v", out)
	}
	adr := addrs[0].(map[string]any)["addr_token"].(string)
	if tokenKind(adr) != "address" {
		t.Fatalf("bad addr_token: %v", adr)
	}

	// Read back — PII is decrypted for the owner.
	code, got := do(t, h, "GET", "/v1/profiles/"+usr, "ID", "")
	if code != 200 || got["full_name"] != "Budi Santoso" || got["phone"] != "+62-812-1111-2222" {
		t.Fatalf("get: %d %v", code, got)
	}

	// Update mutable PII.
	code, _ = do(t, h, "PUT", "/v1/profiles/"+usr, "ID", `{"full_name":"Budi S.","phone":"+62-812-1111-9999","email":"budi@example.co.id"}`)
	if code != 200 {
		t.Fatalf("update: %d", code)
	}
	_, got = do(t, h, "GET", "/v1/profiles/"+usr, "ID", "")
	if got["full_name"] != "Budi S." {
		t.Fatalf("update not applied: %v", got)
	}

	// Add an address → new adr_ token.
	code, av := do(t, h, "POST", "/v1/profiles/"+usr+"/addresses", "ID", `{"label":"work","line1":"Jl. Thamrin 1","city":"Jakarta","postal":"10230"}`)
	if code != 201 || tokenKind(av["addr_token"].(string)) != "address" {
		t.Fatalf("add address: %d %v", code, av)
	}
}

// TestPIICiphertextAtRest — the stored columns in BOTH primary and backup are
// ciphertext; the plaintext PII never appears in any column value.
func TestPIICiphertextAtRest(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cs := s.stores.cell("ID")
	in := profileInput{Jurisdiction: "ID", UserToken: newToken("usr"), FullName: "Siti Rahayu", Phone: "+62-813-7777-8888", Email: "siti@example.co.id",
		Addresses: []addressInput{{Label: "home", Line1: "Jl. Sudirman 5", City: "Bandung", Postal: "40111"}}}
	if _, err := cs.createProfile(ctx, s.kr, in, s.ev); err != nil {
		t.Fatal(err)
	}
	for _, db := range []string{"primary", "backup"} {
		conn := cs.primary
		if db == "backup" {
			conn = cs.backup
		}
		var name, phone, email string
		_ = conn.QueryRowContext(ctx, `SELECT COALESCE(full_name_ct,''), COALESCE(phone_ct,''), COALESCE(email_ct,'') FROM profiles WHERE user_token=?`, in.UserToken).Scan(&name, &phone, &email)
		for _, v := range []string{name, phone, email} {
			if strings.Contains(v, "Siti") || strings.Contains(v, "7777") || strings.Contains(v, "siti@") {
				t.Fatalf("%s store leaks plaintext PII in a column: %q", db, v)
			}
			if v == "" {
				t.Fatalf("%s store column empty (nothing stored)", db)
			}
		}
	}
}

// TestResidencyDeniesNonOwningCell — a process homed only for VN refuses any
// request tagged for ID (403). This is the app-layer twin of the NetworkPolicy
// that denies non-owning-cell PII access.
func TestResidencyDeniesNonOwningCell(t *testing.T) {
	s := newTestServer(t, "VN") // homed ONLY for VN
	h := s.handler()
	// Create for ID via a VN-homed cell → 403 residency.
	code, out := do(t, h, "POST", "/v1/profiles", "ID", `{"jurisdiction":"ID","full_name":"X","phone":"1"}`)
	if code != 403 || errCode(out) != "PROFILE_RESIDENCY_VIOLATION" {
		t.Fatalf("cross-cell create should 403 residency: %d %v", code, out)
	}
	// A read tagged for ID → 403 too.
	code, out = do(t, h, "GET", "/v1/profiles/usr_whatever", "ID", "")
	if code != 403 || errCode(out) != "PROFILE_RESIDENCY_VIOLATION" {
		t.Fatalf("cross-cell read should 403 residency: %d %v", code, out)
	}
	// VN request works (homed).
	code, _ = do(t, h, "POST", "/v1/profiles", "VN", `{"jurisdiction":"VN","full_name":"Nguyen","phone":"+84"}`)
	if code != 201 {
		t.Fatalf("VN create should succeed on a VN cell: %d", code)
	}
}

// TestErasureCryptoShredding is the V-T2 headline proof: after erasure the PII is
// unreadable across the primary store AND the backup, while a token-only order
// snapshot still replays. It is invoked by ci/pii-scan.sh (via -run TestErasure).
func TestErasureCryptoShredding(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cs := s.stores.cell("ID")

	in := profileInput{Jurisdiction: "ID", UserToken: newToken("usr"), FullName: "Budi Santoso", Phone: "+62-812-1111-2222", Email: "budi@example.co.id",
		Addresses: []addressInput{{Label: "home", Line1: "Jl. Merdeka 17", City: "Jakarta", Postal: "10110"}}}
	pv, err := cs.createProfile(ctx, s.kr, in, s.ev)
	if err != nil {
		t.Fatal(err)
	}
	usr := in.UserToken
	adr := pv.Addresses[0].AddrToken

	// Before erasure: readable from primary AND backup.
	if got, err := cs.getProfile(ctx, s.kr, usr); err != nil || got.FullName != "Budi Santoso" {
		t.Fatalf("pre-erase primary read: %v %v", got, err)
	}
	if got, err := cs.getProfileFromBackup(ctx, s.kr, usr); err != nil || got.FullName != "Budi Santoso" {
		t.Fatalf("pre-erase backup read: %v %v", got, err)
	}

	// A token-only order snapshot (the shape on the order log). Assert NO PII.
	snap := orderSnapshot{OrderToken: newToken("ord"), UserToken: usr, AddrToken: adr, Jurisdiction: "ID",
		Items: []lineItem{{SKU: "sku_nasi", Qty: 2, PriceMinor: 4500}, {SKU: "sku_es", Qty: 1, PriceMinor: 1500}}, Currency: "IDR"}
	snapJSON, _ := json.Marshal(snap)
	for _, pii := range []string{"Budi", "1111-2222", "Merdeka", "budi@"} {
		if bytes.Contains(snapJSON, []byte(pii)) {
			t.Fatalf("order snapshot leaked PII %q: %s", pii, snapJSON)
		}
	}

	// Replay BEFORE erasure works.
	if _, err := replayOrder(ctx, s.stores, snap); err != nil {
		t.Fatalf("pre-erase replay: %v", err)
	}

	// Capture the raw backup ciphertext — it must physically survive erasure.
	backupCT, err := cs.rawBackupCiphertext(ctx, usr)
	if err != nil || backupCT == "" {
		t.Fatalf("no backup ciphertext to shred against: %q %v", backupCT, err)
	}

	// ---- ERASE (crypto-shred) ----
	receipt, err := cs.erase(ctx, usr, s.ev)
	if err != nil {
		t.Fatalf("erase: %v", err)
	}
	if !receipt.KeyDestroyed || len(receipt.StoresShred) != 2 {
		t.Fatalf("erase receipt: %+v", receipt)
	}

	// PII unreadable from PRIMARY.
	if _, err := cs.getProfile(ctx, s.kr, usr); err != errKeyDestroyed {
		t.Fatalf("primary readable after erasure (want errKeyDestroyed): %v", err)
	}
	// PII unreadable from BACKUP — the ciphertext is still there, but no DEK exists.
	stillThere, _ := cs.rawBackupCiphertext(ctx, usr)
	if stillThere != backupCT {
		t.Fatalf("backup ciphertext changed; crypto-shredding must NOT need to touch immutable backups")
	}
	if _, err := cs.getProfileFromBackup(ctx, s.kr, usr); err != errKeyDestroyed {
		t.Fatalf("backup readable after erasure (want errKeyDestroyed): %v", err)
	}

	// Yet the token-only order history STILL replays (tokens survive as valid refs).
	out, err := replayOrder(ctx, s.stores, snap)
	if err != nil {
		t.Fatalf("post-erase replay failed: %v", err)
	}
	if out.TotalMinor != 2*4500+1500 || out.Currency != "IDR" {
		t.Fatalf("replay total wrong: %+v", out)
	}
	if !out.UserRef.Exists || !out.UserRef.Erased {
		t.Fatalf("post-erase user ref should exist+erased: %+v", out.UserRef)
	}
	if out.UserRef.Jurisdiction != "ID" {
		t.Fatalf("token still resolves to owning cell: %+v", out.UserRef)
	}
	t.Logf("ERASURE PROOF: usr=%s erased; primary+backup PII unreadable (errKeyDestroyed); order %s replayed total=%d %s with token refs (user erased=%v)",
		usr, out.OrderToken, out.TotalMinor, out.Currency, out.UserRef.Erased)
}

// TestEventsAreTokenOnly — every event the outbox holds is token-only (no PII).
func TestEventsAreTokenOnly(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cs := s.stores.cell("ID")
	in := profileInput{Jurisdiction: "ID", UserToken: newToken("usr"), FullName: "Tan Wei Ming", Phone: "+65-9123-4567", Email: "tan@example.sg",
		Addresses: []addressInput{{Label: "home", Line1: "8 Marina Blvd", City: "Singapore", Postal: "018981"}}}
	if _, err := cs.createProfile(ctx, s.kr, in, s.ev); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.erase(ctx, in.UserToken, s.ev); err != nil {
		t.Fatal(err)
	}
	recs, err := cs.ob.Tail(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 2 {
		t.Fatalf("expected >=2 events (created+erased), got %d", len(recs))
	}
	pii := []string{"Tan Wei", "9123-4567", "Marina", "tan@", "Singapore"}
	sawUsr, sawErased := false, false
	for _, r := range recs {
		for _, p := range pii {
			if bytes.Contains(r.Raw, []byte(p)) {
				t.Fatalf("event leaked PII %q: %s", p, r.Raw)
			}
		}
		if bytes.Contains(r.Raw, []byte(in.UserToken)) {
			sawUsr = true
		}
		if bytes.Contains(r.Raw, []byte(`"profile.erased"`)) {
			sawErased = true
		}
	}
	if !sawUsr || !sawErased {
		t.Fatalf("events missing usr token (%v) or erased type (%v)", sawUsr, sawErased)
	}
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}
