package errors

import (
	stderrors "errors"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistryMappings(t *testing.T) {
	cases := []struct {
		code   string
		status int
		retry  bool
	}{
		{CodeValidation, 400, false},
		{CodeNotFound, 404, false},
		{CodeConflict, 409, false},
		{CodeDomainRule, 422, false},
		{CodeRateLimited, 429, true},
		{CodeInternal, 500, false},
		{CodeIdempotencyKeyReuse, 409, false},
		{CodeIdempotencyInProgress, 409, true},
		{CodeIdempotencyKeyRequired, 400, false},
	}
	for _, c := range cases {
		if got := HTTPStatus(c.code); got != c.status {
			t.Errorf("%s: status=%d want %d", c.code, got, c.status)
		}
		if got := Retryable(c.code); got != c.retry {
			t.Errorf("%s: retryable=%v want %v", c.code, got, c.retry)
		}
	}
}

func TestAllCodesUpperSnake(t *testing.T) {
	for _, c := range Codes() {
		if !isUpperSnake(c) {
			t.Errorf("registered code %q is not UPPER_SNAKE", c)
		}
	}
}

func TestRegisterRejectsBadCode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on non-UPPER_SNAKE code")
		}
	}()
	Register("bad-code", 400, false, "x")
}

func TestEnvelopeShape(t *testing.T) {
	err := New(CodeConflict, "cannot move DELIVERED to CANCELLED",
		Detail{Field: "status", Reason: "terminal_state"})
	status, env := ToEnvelope(err, "4bf92f3577b34da6a3ce929d0e0e4736")
	if status != 409 {
		t.Fatalf("status=%d want 409", status)
	}
	if env.Error.Code != "CONFLICT" || env.Error.Retryable {
		t.Fatalf("bad envelope: %+v", env.Error)
	}
	if len(env.Error.Details) != 1 || env.Error.Details[0].Field != "status" {
		t.Fatalf("details lost: %+v", env.Error.Details)
	}
	// Round-trips through JSON with the exact 02 §2 keys.
	b, _ := json.Marshal(env)
	var raw map[string]map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"code", "message", "details", "trace_id", "retryable"} {
		if _, ok := raw["error"][k]; !ok {
			t.Errorf("envelope missing key %q; got %s", k, b)
		}
	}
}

func TestUnknownErrorBecomesInternal(t *testing.T) {
	status, env := ToEnvelope(stderrors.New("boom"), "tid")
	if status != 500 || env.Error.Code != "INTERNAL" {
		t.Fatalf("unknown error not mapped to INTERNAL: %d %s", status, env.Error.Code)
	}
	if env.Error.Details == nil {
		t.Fatal("details must serialise as [] not null")
	}
}

func TestIsMatchesByCode(t *testing.T) {
	e := Wrap(CodeNotFound, stderrors.New("row missing"), "")
	if !stderrors.Is(e, New(CodeNotFound, "")) {
		t.Fatal("errors.Is should match by code")
	}
	if stderrors.Is(e, New(CodeConflict, "")) {
		t.Fatal("errors.Is should not match a different code")
	}
}

func TestWriteHTTP(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New(CodeRateLimited, ""), "tid123")
	if rec.Code != 429 {
		t.Fatalf("status=%d want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q", ct)
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.TraceID != "tid123" || !env.Error.Retryable {
		t.Fatalf("bad body: %+v", env.Error)
	}
}

var _ http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
