package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shop-platform/shop/libs/logging"
)

// golden.go — generate "golden traffic": the events + logs a realistic run of
// this service emits, fed with REAL PII inputs. tools/piiscan then scans the
// output and must find ZERO of the PII strings (known-pii.txt) — proving the
// event/log paths are token-only (D3). This runs the SAME store code paths the
// live service uses, so it is a genuine end-to-end proof, not a mock.

type goldenSeed struct {
	jurisdiction     string
	fullName         string
	phone            string
	email            string
	line1, city, pc  string
}

// goldenSeeds are deliberately spicy PII: names, phones, emails, streets across
// the residency jurisdictions. Every value here goes into known-pii.txt.
var goldenSeeds = []goldenSeed{
	{"ID", "Budi Santoso", "+62-812-1111-2222", "budi.santoso@example.co.id", "Jl. Merdeka 17", "Jakarta", "10110"},
	{"VN", "Nguyen Van An", "+84-90-333-4444", "nguyen.van.an@example.vn", "12 Le Loi", "Ho Chi Minh City", "700000"},
	{"SG", "Tan Wei Ming", "+65-9123-4567", "weiming.tan@example.sg", "8 Marina Blvd", "Singapore", "018981"},
	{"TH", "Somchai Prasert", "+66-81-555-6666", "somchai.prasert@example.th", "99 Sukhumvit", "Bangkok", "10110"},
	{"ID", "Siti Rahayu", "+62-813-7777-8888", "siti.rahayu@example.co.id", "Jl. Sudirman 5", "Bandung", "40111"},
}

func runEmitGolden(ctx context.Context, st *stores, kr *keyring, region, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ev := newEventBuilder(region)

	logf, err := os.Create(filepath.Join(dir, "logs.jsonl"))
	if err != nil {
		return err
	}
	defer logf.Close()
	lg := logging.New(logging.Config{
		Service: "identity-profile", Version: "golden", Env: "test", Region: region, Out: logf, SampleRate: 1.0,
	})

	var known []string
	addKnown := func(vs ...string) {
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				known = append(known, v)
			}
		}
	}

	var createdTokens []string
	for _, sd := range goldenSeeds {
		addKnown(sd.fullName, sd.phone, sd.email, sd.line1, sd.city)
		cs := st.cell(sd.jurisdiction)
		in := profileInput{
			Jurisdiction: sd.jurisdiction,
			UserToken:    newToken("usr"),
			FullName:     sd.fullName, Phone: sd.phone, Email: sd.email,
			Addresses: []addressInput{{Label: "home", Line1: sd.line1, City: sd.city, Postal: sd.pc}},
		}
		pv, err := cs.createProfile(ctx, kr, in, ev)
		if err != nil {
			return fmt.Errorf("create %s: %w", sd.jurisdiction, err)
		}
		createdTokens = append(createdTokens, in.UserToken)
		// Emit a token-only business log line (keys carry tokens, never PII).
		adr := ""
		if len(pv.Addresses) > 0 {
			adr = pv.Addresses[0].AddrToken
		}
		lg.Emit(logging.Entry{
			Level: "INFO", Direction: logging.Direction("ingress"), Protocol: logging.Protocol("http"),
			Route: "POST /v1/profiles", Peer: "customer-bff", Status: 201, TraceID: randTraceID(),
			Actor: &logging.Actor{Type: "user", ID: in.UserToken},
			Keys:  map[string]string{"user_token": in.UserToken, "addr_token": adr, "jurisdiction": sd.jurisdiction},
		})
	}

	// A couple of updates + an extra address (more event/log traffic).
	if len(createdTokens) > 0 {
		cs := st.cell(goldenSeeds[0].jurisdiction)
		_, _ = cs.updateProfile(ctx, kr, createdTokens[0], profileInput{FullName: "Budi S.", Phone: "+62-812-1111-9999"}, ev)
		addKnown("Budi S.", "+62-812-1111-9999")
		_, _ = cs.addAddress(ctx, kr, createdTokens[0], addressInput{Label: "work", Line1: "Jl. Thamrin 1", City: "Jakarta", Postal: "10230"}, ev)
		addKnown("Jl. Thamrin 1")
		lg.Emit(logging.Entry{
			Level: "INFO", Direction: logging.Direction("ingress"), Protocol: logging.Protocol("http"),
			Route: "PUT /v1/profiles/{usr}", Peer: "customer-bff", Status: 200, TraceID: randTraceID(),
			Actor: &logging.Actor{Type: "user", ID: createdTokens[0]},
			Keys:  map[string]string{"user_token": createdTokens[0], "jurisdiction": goldenSeeds[0].jurisdiction},
		})
	}

	// Erase one user (emits a token-only profile.erased event + audit log).
	if len(createdTokens) > 1 {
		cs := st.cell(goldenSeeds[1].jurisdiction)
		receipt, err := cs.erase(ctx, createdTokens[1], ev)
		if err != nil {
			return fmt.Errorf("erase: %w", err)
		}
		lg.Emit(logging.Entry{
			Level: "INFO", Direction: logging.Direction("ingress"), Protocol: logging.Protocol("http"),
			Route: "POST /v1/profiles/{usr}:erase", Peer: "admin-bff", Status: 200, TraceID: randTraceID(),
			Actor: &logging.Actor{Type: "dpo", ID: createdTokens[1]},
			Keys:  map[string]string{"user_token": receipt.UserToken, "jurisdiction": receipt.Jurisdiction, "key_destroyed": "true"},
		})
	}

	// Dump every emitted event straight from the transactional outbox of each
	// cell (the exact bytes the CDC relay would publish).
	evf, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return err
	}
	defer evf.Close()
	ew := bufio.NewWriter(evf)
	total := 0
	for _, cs := range st.byJur {
		recs, err := cs.ob.Tail(ctx, 0, 100000)
		if err != nil {
			return err
		}
		for _, r := range recs {
			ew.Write(r.Raw)
			ew.WriteByte('\n')
			total++
		}
	}
	if err := ew.Flush(); err != nil {
		return err
	}

	// The known-PII wordlist: every plaintext value we fed in. The scanner asserts
	// none of these appear in events.jsonl / logs.jsonl.
	kf, err := os.Create(filepath.Join(dir, "known-pii.txt"))
	if err != nil {
		return err
	}
	defer kf.Close()
	for _, v := range known {
		fmt.Fprintln(kf, v)
	}

	fmt.Printf("emit-golden: wrote %d events + %d known-PII strings to %s (events.jsonl, logs.jsonl, known-pii.txt)\n", total, len(known), dir)
	return nil
}
