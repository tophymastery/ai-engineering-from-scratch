// Edge authentication (V-T1 / D4). The gateway verifies short-lived ES256
// access tokens LOCALLY — cached JWKS, no per-request call to identity-auth —
// and checks each token's jti against a bloom denylist it polls ≤30s. This is
// the "no introspection on the hot path" half of D4: identity-auth can be down
// and previously-issued tokens keep verifying here.
//
// Two independent duties, both always active regardless of the flag:
//   - spoof defence: X-Auth-Subject / X-Auth-Role are STRIPPED from every
//     inbound request before proxying, so upstreams can trust them as
//     gateway-asserted identity (02 §1: "services receive signed identity
//     headers, never raw tokens").
// When FLAG auth_jwt_edge is on and a request carries `Authorization: Bearer`,
// the token is verified; on success the subject/role are re-injected as those
// headers; on failure the request is rejected with the 02 §2 envelope
// (401 AUTH_TOKEN_INVALID / AUTH_TOKEN_REVOKED). A request with no bearer token
// passes through unauthenticated — per-route auth requirements are the
// service's/BFF's job; the gateway only asserts identity when a token is shown.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shop-platform/shop/libs/edgeauth"
	shoperr "github.com/shop-platform/shop/libs/errors"
	"github.com/shop-platform/shop/libs/otel"
)

const (
	hdrAuthSubject = "X-Auth-Subject"
	hdrAuthRole    = "X-Auth-Role"
)

// edgeAuth is the gateway's verifier: a JWKS cache + a polled denylist.
type edgeAuth struct {
	enabled      bool
	identityBase string // e.g. http://localhost:8101 (JWKS + denylist source)
	pollEvery    time.Duration
	leeway       time.Duration
	client       *http.Client
	jwks         *jwksCache
	deny         *denyCache
}

func newEdgeAuth(enabled bool, identityBase string, pollEvery time.Duration) *edgeAuth {
	c := &http.Client{Timeout: 3 * time.Second}
	return &edgeAuth{
		enabled:      enabled,
		identityBase: strings.TrimRight(identityBase, "/"),
		pollEvery:    pollEvery,
		leeway:       60 * time.Second, // absorb per-cell clock skew (D4)
		client:       c,
		jwks:         newJWKSCache(c, strings.TrimRight(identityBase, "/")+"/.well-known/jwks.json"),
		deny:         newDenyCache(c, strings.TrimRight(identityBase, "/")+"/v1/auth/denylist"),
	}
}

// start warms the caches and launches the denylist poller (≤30s revocation lag,
// DENYLIST_POLL here defaults to 5s). JWKS is refreshed lazily on an unknown kid
// and also warmed once here; a failure is non-fatal (cache stays empty until a
// token actually needs verifying).
func (a *edgeAuth) start(ctx context.Context) {
	if !a.enabled {
		return
	}
	_ = a.jwks.refresh()   // best-effort warm; unknown-kid refresh covers misses
	_ = a.deny.pollOnce()  // best-effort warm
	go a.deny.pollLoop(ctx, a.pollEvery)
}

// verify checks signature+time (from cached JWKS, refreshing once on an unknown
// kid) then revocation (bloom denylist). It returns claims or a registered error.
func (a *edgeAuth) verify(token string) (edgeauth.Claims, error) {
	claims, err := edgeauth.Verify(token, a.jwks.lookup, time.Now(), a.leeway)
	if err != nil {
		return edgeauth.Claims{}, err
	}
	if a.deny.test(claims.JTI) {
		return edgeauth.Claims{}, shoperr.New(edgeauth.CodeTokenRevoked, "")
	}
	return claims, nil
}

// middleware is the outermost auth layer. It ALWAYS strips spoofed identity
// headers; when enabled it verifies a presented bearer token and re-injects the
// asserted identity. No bearer token ⇒ anonymous passthrough.
func (a *edgeAuth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Spoof defence runs unconditionally so an upstream can NEVER receive a
		// client-forged identity header, flag on or off.
		r.Header.Del(hdrAuthSubject)
		r.Header.Del(hdrAuthRole)

		if a.enabled {
			if tok, ok := bearer(r); ok {
				claims, err := a.verify(tok)
				if err != nil {
					writeAuthError(w, r, err)
					return
				}
				r.Header.Set(hdrAuthSubject, claims.Sub)
				r.Header.Set(hdrAuthRole, claims.Role)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):]), true
	}
	return "", false
}

// writeAuthError serialises the 02 §2 envelope with the live trace id.
func writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	tid := ""
	if sc, ok := otel.Extract(r); ok {
		tid = sc.TraceIDHex()
	}
	shoperr.Write(w, err, tid)
}

// ---- JWKS cache ---------------------------------------------------------

type jwksCache struct {
	url         string
	client      *http.Client
	mu          sync.RWMutex
	keys        map[string]*ecdsa.PublicKey
	lastRefresh time.Time
}

func newJWKSCache(c *http.Client, url string) *jwksCache {
	return &jwksCache{url: url, client: c, keys: map[string]*ecdsa.PublicKey{}}
}

// lookup returns the key for a kid. On a miss it triggers ONE throttled refresh
// (an unknown kid means a rotation just happened) and retries. Refresh failures
// leave the existing cache intact — an identity outage must not drop known keys.
func (j *jwksCache) lookup(kid string) (*ecdsa.PublicKey, bool) {
	j.mu.RLock()
	k, ok := j.keys[kid]
	j.mu.RUnlock()
	if ok {
		return k, true
	}
	// Unknown kid: refresh at most once per second to avoid a thundering herd on
	// a genuinely-bad kid (forged tokens) while still picking up a freshly-rotated
	// key promptly (≤1s added to the first request that carries the new kid).
	j.mu.Lock()
	stale := time.Since(j.lastRefresh) > time.Second
	j.mu.Unlock()
	if stale {
		_ = j.refresh()
		j.mu.RLock()
		k, ok = j.keys[kid]
		j.mu.RUnlock()
	}
	return k, ok
}

func (j *jwksCache) refresh() error {
	resp, err := j.client.Get(j.url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errUnexpectedStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	keys, err := edgeauth.ParseJWKS(body)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.keys = keys
	j.lastRefresh = time.Now()
	j.mu.Unlock()
	return nil
}

func (j *jwksCache) size() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.keys)
}

// ---- denylist cache -----------------------------------------------------

type denyCache struct {
	url     string
	client  *http.Client
	mu      sync.RWMutex
	bloom   *edgeauth.Bloom
	version uint64
	fails   int // consecutive poll failures (observability / alerting seam)
}

func newDenyCache(c *http.Client, url string) *denyCache {
	return &denyCache{url: url, client: c}
}

// test reports whether a jti is (possibly) revoked. An empty/never-fetched cache
// returns false — fail-open on the denylist is deliberate: availability of the
// denylist source must not take down authenticated traffic (D4). Signature
// verification is the always-on gate; the denylist only adds ≤30s revocation.
func (d *denyCache) test(jti string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.bloom == nil {
		return false
	}
	return d.bloom.Test(jti)
}

func (d *denyCache) pollOnce() error {
	resp, err := d.client.Get(d.url)
	if err != nil {
		d.recordFail()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.recordFail()
		return errUnexpectedStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		d.recordFail()
		return err
	}
	var snap edgeauth.BloomSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		d.recordFail()
		return err
	}
	bloom, err := edgeauth.BloomFromSnapshot(snap)
	if err != nil {
		d.recordFail()
		return err
	}
	d.mu.Lock()
	d.bloom = bloom
	d.version = snap.Version
	d.fails = 0
	d.mu.Unlock()
	return nil
}

func (d *denyCache) recordFail() {
	d.mu.Lock()
	d.fails++
	d.mu.Unlock()
}

func (d *denyCache) pollLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.pollOnce(); err != nil {
				log.Printf("gateway: denylist poll failed: %v", err)
			}
		}
	}
}

func (d *denyCache) currentVersion() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.version
}

type unexpectedStatus int

func (u unexpectedStatus) Error() string { return "unexpected status " + itoa(int(u)) }
func errUnexpectedStatus(c int) error    { return unexpectedStatus(c) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
