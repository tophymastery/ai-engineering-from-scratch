// Package flags is the env-backed feature-flag lookup with a per-request
// override for non-prod.
//
// Baseline value: environment variable FLAG_<NAME> (e.g. FLAG_SAGA_V1=true).
// Every slice ships flag-gated (TASKS.md) so merge order is irrelevant.
//
// Per-request override (D29): a request may carry X-Flag-Override to force flag
// values for a single request — deterministic tests and preview envs. This is
// honoured ONLY in non-prod builds: the override value is read exclusively
// through libs/testhooks, whose reader is a pure passthrough (always "") in a
// production build because hooks_enabled.go is compiled out. So in prod the
// override path is not merely disabled — the code that reads the header does
// not exist in the binary (ci/backdoor-scan.sh proves it). The gateway strips
// the header in prod as a second, independent layer.
package flags

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/shop-platform/shop/libs/testhooks"
)

// OverrideHeader is the request header carrying per-request flag overrides,
// formatted as a comma-separated list of name=value pairs:
//
//	X-Flag-Override: saga_v1=true,pricing_v1=false
//
// It mirrors testhooks' header name. The literal is duplicated here (rather than
// referenced) because the testhooks constant exists only in the non-prod build;
// the gateway strip rule and testhooks reader key on the same name.
const OverrideHeader = "X-Flag-Override"

// Set is a snapshot of environment-backed flag defaults. Construct with
// FromEnv; it is immutable and safe for concurrent reads.
type Set struct {
	env map[string]string
}

// FromEnv reads all FLAG_* environment variables into a Set. Names are
// normalised to lower_snake (FLAG_SAGA_V1 → "saga_v1").
func FromEnv() *Set {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		if !strings.HasPrefix(k, "FLAG_") {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(k, "FLAG_"))
		m[name] = v
	}
	return &Set{env: m}
}

// NewSet builds a Set from an explicit map (for tests / config injection).
func NewSet(m map[string]string) *Set {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[strings.ToLower(k)] = v
	}
	return &Set{env: cp}
}

// Bool returns the environment-backed value of a flag (no per-request override).
func (s *Set) Bool(name string, def bool) bool {
	if v, ok := s.env[strings.ToLower(name)]; ok {
		return truthy(v, def)
	}
	return def
}

// String returns the environment-backed string value of a flag.
func (s *Set) String(name, def string) string {
	if v, ok := s.env[strings.ToLower(name)]; ok {
		return v
	}
	return def
}

// BoolCtx returns a flag value, applying any per-request override present in
// ctx FIRST (non-prod only), then falling back to the environment default.
// In a production build the override lookup always misses, so this equals Bool.
func (s *Set) BoolCtx(ctx context.Context, name string, def bool) bool {
	if v, ok := overrideFromContext(ctx, name); ok {
		return truthy(v, def)
	}
	return s.Bool(name, def)
}

// StringCtx is the string analogue of BoolCtx.
func (s *Set) StringCtx(ctx context.Context, name, def string) string {
	if v, ok := overrideFromContext(ctx, name); ok {
		return v
	}
	return s.String(name, def)
}

// OverrideActive reports whether this build can honour per-request overrides at
// all — i.e. whether testhooks are compiled in. False in prod builds. Useful
// for a health/debug endpoint and for the prod-refusal test.
func OverrideActive() bool { return testhooks.Enabled }

// overrideFromContext parses the X-Flag-Override value stashed in ctx by
// testhooks.Middleware and returns the value for name. In a prod build
// testhooks.FlagOverrideFromContext always returns ("", false), so no override
// is ever applied and this returns ("", false).
func overrideFromContext(ctx context.Context, name string) (string, bool) {
	raw, ok := testhooks.FlagOverrideFromContext(ctx)
	if !ok || raw == "" {
		return "", false
	}
	return parseOverride(raw, strings.ToLower(name))
}

// parseOverride finds name in a "a=1,b=0" override string.
func parseOverride(raw, name string) (string, bool) {
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		i := strings.IndexByte(pair, '=')
		if i < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(pair[:i])) == name {
			return strings.TrimSpace(pair[i+1:]), true
		}
	}
	return "", false
}

func truthy(v string, def bool) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}
