# libs/errors

Stable UPPER_SNAKE **error-code registry** and the **02 §2 error envelope** with
HTTP-status mapping. Every non-2xx from every service/BFF serialises through here
— one shape, one code vocabulary.

## Envelope (02 §2)

```json
{"error":{"code":"ORDER_INVALID_TRANSITION","message":"…","details":[{"field":"status","reason":"terminal_state"}],"trace_id":"4bf9…","retryable":false}}
```

- `code` — stable, machine-readable, UPPER_SNAKE, registered once. Clients switch on it.
- `message` — human-only, may change freely.
- `trace_id` — the live trace (04 §2) so a user report resolves to a trace in one hop.
- `retryable` — tells clients which errors are safe to retry.

## Usage

```go
// register a domain code once (panics if not UPPER_SNAKE / bad status):
var CodeOrderInvalidTransition = errors.Register("ORDER_INVALID_TRANSITION", 409, false, "…")

// build + write:
err := errors.New(CodeOrderInvalidTransition, "ord_… cannot move DELIVERED→CANCELLED",
    errors.Detail{Field: "status", Reason: "terminal_state"})
errors.Write(w, err, traceID)          // status + JSON envelope
errors.WriteRequest(w, r, err, logging.TraceIDFromRequest) // trace_id from ctx
```

`errors.Is` matches by code. Any non-`*errors.Error` written through `ToEnvelope`
becomes `INTERNAL` (500), so a handler can never leak a non-conforming body.

## HTTP mapping (registry baseline)

`VALIDATION` 400 · `UNAUTHENTICATED` 401 · `FORBIDDEN` 403 · `NOT_FOUND` 404 ·
`CONFLICT` 409 · `STALE_WRITE` 412 · `DOMAIN_RULE` 422 · `RATE_LIMITED` 429 ·
`INTERNAL` 500 · `UNAVAILABLE` 503 · idempotency: `IDEMPOTENCY_KEY_REQUIRED` 400,
`IDEMPOTENCY_KEY_REUSED` 409, `IDEMPOTENCY_IN_PROGRESS` 409 (retryable).

Zero external dependencies (stdlib only).
