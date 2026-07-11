package eventbus

import (
	"sort"
	"sync"
	"time"
)

// LagRecorder collects relay/consumer lag samples and reports quantiles. It is
// the measurement behind the "relay lag p99 < 2s" criterion (S-T6). Samples are
// durations; the recorder is safe for concurrent Record from many partition
// workers.
type LagRecorder struct {
	mu      sync.Mutex
	samples []time.Duration
	max     time.Duration
	count   int64
}

// NewLagRecorder builds an empty recorder. hint pre-sizes the sample buffer.
func NewLagRecorder(hint int) *LagRecorder {
	if hint < 0 {
		hint = 0
	}
	return &LagRecorder{samples: make([]time.Duration, 0, hint)}
}

// Record adds one lag sample.
func (r *LagRecorder) Record(d time.Duration) {
	r.mu.Lock()
	r.samples = append(r.samples, d)
	if d > r.max {
		r.max = d
	}
	r.count++
	r.mu.Unlock()
}

// Count is the number of samples recorded.
func (r *LagRecorder) Count() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Max is the largest lag observed.
func (r *LagRecorder) Max() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max
}

// Quantile returns the q-th (0..1) quantile of recorded lag. Computed by
// sorting a copy; called once at end-of-soak, not on the hot path.
func (r *LagRecorder) Quantile(q float64) time.Duration {
	r.mu.Lock()
	cp := make([]time.Duration, len(r.samples))
	copy(cp, r.samples)
	r.mu.Unlock()
	if len(cp) == 0 {
		return 0
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(q * float64(len(cp)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}
