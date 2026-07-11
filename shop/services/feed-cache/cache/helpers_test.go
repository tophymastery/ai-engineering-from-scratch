package cache

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// countingOrigin is the test origin the correctness proofs count against: every
// Fetch increments an ATOMIC counter, so "exactly 1 origin fetch under a 10k
// stampede" is a real, observed count — not a mock expectation. An optional gate
// channel holds the leader's fetch in-flight so a concurrent stampede has time to
// pile onto the singleflight before the leader completes (the reliable way to
// make the collapse deterministic under -race). An optional delay adds the same
// effect via a short sleep.
type countingOrigin struct {
	fetches atomic.Int64
	seq     atomic.Int64  // monotonically labels each fetched value (revalidation proof)
	gate    chan struct{} // if non-nil, Fetch blocks until it is closed
	delay   time.Duration
	lastHdr atomic.Value // http.Header of the most recent fetch (forwarding proof)
}

func (o *countingOrigin) Fetch(_ context.Context, key string, hdr http.Header) ([]byte, error) {
	if o.gate != nil {
		<-o.gate
	}
	if o.delay > 0 {
		time.Sleep(o.delay)
	}
	n := o.seq.Add(1)
	o.fetches.Add(1)
	if hdr != nil {
		o.lastHdr.Store(hdr.Clone())
	}
	return []byte(fmt.Sprintf(`{"key":%q,"v":%d}`, key, n)), nil
}

func (o *countingOrigin) count() int64 { return o.fetches.Load() }
