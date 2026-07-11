package flags

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shop-platform/shop/libs/testhooks"
)

func TestEnvBacked(t *testing.T) {
	s := NewSet(map[string]string{"SAGA_V1": "true", "PRICING_V1": "false", "MODE": "shadow"})
	if !s.Bool("saga_v1", false) {
		t.Error("saga_v1 should be true from env")
	}
	if s.Bool("pricing_v1", true) {
		t.Error("pricing_v1 should be false from env")
	}
	if s.Bool("unknown", true) != true {
		t.Error("unknown flag should return default")
	}
	if s.String("mode", "enforce") != "shadow" {
		t.Error("string flag from env wrong")
	}
	if s.String("missing", "def") != "def" {
		t.Error("missing string flag should return default")
	}
}

func TestParseOverride(t *testing.T) {
	raw := "saga_v1=true, pricing_v1=false ,mode=enforce"
	if v, ok := parseOverride(raw, "saga_v1"); !ok || v != "true" {
		t.Errorf("saga_v1 override=%q ok=%v", v, ok)
	}
	if v, ok := parseOverride(raw, "pricing_v1"); !ok || v != "false" {
		t.Errorf("pricing_v1 override=%q ok=%v", v, ok)
	}
	if _, ok := parseOverride(raw, "absent"); ok {
		t.Error("absent flag should not be found")
	}
}

// The per-request override is honoured ONLY in non-prod (testhooks) builds. This
// single test asserts the correct behaviour for whichever build tag is active,
// so `go test` (prod) and `go test -tags testhooks` (non-prod) both pass and
// together prove both halves of D29.
func TestOverridePerRequest(t *testing.T) {
	s := NewSet(map[string]string{"saga_v1": "false"}) // env default: OFF

	var got bool
	var active bool
	h := testhooks.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = s.BoolCtx(r.Context(), "saga_v1", false)
		active = OverrideActive()
	}))
	req := httptest.NewRequest("POST", "/kv", nil)
	req.Header.Set(OverrideHeader, "saga_v1=true") // force ON for this request
	h.ServeHTTP(httptest.NewRecorder(), req)

	if testhooks.Enabled {
		// non-prod build: override wins over the OFF env default.
		if !got {
			t.Fatal("non-prod: X-Flag-Override should force saga_v1 ON")
		}
		if !active {
			t.Fatal("non-prod: OverrideActive() should be true")
		}
	} else {
		// prod build: the override is refused — env default (OFF) stands, and
		// the override machinery is compiled out.
		if got {
			t.Fatal("prod: X-Flag-Override must be IGNORED (env default stands)")
		}
		if active {
			t.Fatal("prod: OverrideActive() should be false")
		}
	}
}

// Without any override in context, BoolCtx == Bool in every build.
func TestBoolCtxFallsBackToEnv(t *testing.T) {
	s := NewSet(map[string]string{"x": "true"})
	if s.BoolCtx(context.Background(), "x", false) != true {
		t.Fatal("BoolCtx with no override should read env default")
	}
}
