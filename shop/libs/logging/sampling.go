package logging

import (
	"math/rand"
	"net/http"
)

// Sampler decides whether a given log entry is kept. Per 04 §3 / D27:
//   - mutations (POST/PATCH/DELETE/PUT) → always kept
//   - errors (status >= 400) and WARN/ERROR levels → always kept
//   - read paths (GET/HEAD/OPTIONS, 2xx/3xx) → sampled at a rate
//
// This is the "per-route sampling class" seam: a service can supply its own
// Sampler to keep 100% of a critical read path or drop an ultra-hot one.
type Sampler interface {
	// Keep decides for entry e given a random draw r in [0,1).
	Keep(e Entry, r float64) bool
}

// SamplerFunc adapts a function to Sampler.
type SamplerFunc func(e Entry, r float64) bool

func (f SamplerFunc) Keep(e Entry, r float64) bool { return f(e, r) }

// Class categorises an entry for sampling.
type Class int

const (
	// ClassMutation is any write; always logged (money/state changes must be
	// fully auditable).
	ClassMutation Class = iota
	// ClassError is any failed request (status >= 400) or WARN+/ERROR level;
	// always logged — errors are never sampled (04 §3).
	ClassError
	// ClassRead is a successful read path; eligible for sampling.
	ClassRead
)

// Classify returns the sampling class for an entry from its method (encoded in
// route as "METHOD path") and status/level.
func Classify(e Entry) Class {
	if e.Status >= 400 || e.Level == "ERROR" || e.Level == "WARN" {
		return ClassError
	}
	if isMutationRoute(e.Route) {
		return ClassMutation
	}
	return ClassRead
}

func isMutationRoute(route string) bool {
	// route is "METHOD /path"; the leading token is the HTTP method.
	for i := 0; i < len(route); i++ {
		if route[i] == ' ' {
			m := route[:i]
			return isMutationMethod(m)
		}
	}
	return isMutationMethod(route)
}

func isMutationMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// DefaultSampler keeps all mutations and errors, and keeps read-path lines with
// probability rate (rate>=1 keeps everything).
func DefaultSampler(rate float64) Sampler {
	return SamplerFunc(func(e Entry, r float64) bool {
		switch Classify(e) {
		case ClassMutation, ClassError:
			return true
		default: // ClassRead
			if rate >= 1.0 {
				return true
			}
			if rate <= 0 {
				return false
			}
			return r < rate
		}
	})
}

func defaultRand() float64 { return rand.Float64() }
