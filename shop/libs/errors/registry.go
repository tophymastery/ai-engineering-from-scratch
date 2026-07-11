package errors

// Standard platform codes. Each is registered once, here, with its 02 §2 HTTP
// mapping and retryable semantics. Slices add their own domain codes with their
// own Register() calls at package init — this set is the shared baseline every
// service can rely on.
//
// HTTP mapping (02 §2): 400 validation, 401/403 auth, 404 missing, 409
// conflict/invalid transition, 422 domain rule, 429 rate limit, 5xx server.
var (
	// Generic.
	CodeValidation     = Register("VALIDATION", 400, false, "The request was malformed or failed validation.")
	CodeUnauthenticated = Register("UNAUTHENTICATED", 401, false, "Authentication is required.")
	CodeForbidden      = Register("FORBIDDEN", 403, false, "You do not have permission to perform this action.")
	CodeNotFound       = Register("NOT_FOUND", 404, false, "The requested resource does not exist.")
	CodeConflict       = Register("CONFLICT", 409, false, "The request conflicts with the current state of the resource.")
	CodeStaleWrite     = Register("STALE_WRITE", 412, false, "The resource was modified; retry with the current version (ETag mismatch).")
	CodeDomainRule     = Register("DOMAIN_RULE", 422, false, "The request violates a domain rule.")
	CodeRateLimited    = Register("RATE_LIMITED", 429, true, "Too many requests; slow down and retry.")
	CodeInternal       = Register("INTERNAL", 500, false, "An internal error occurred.")
	CodeUnavailable    = Register("UNAVAILABLE", 503, true, "The service is temporarily unavailable; retry shortly.")

	// Idempotency (02 §3 wire protocol / D9). REUSE is a client bug (same key,
	// different body) — never retryable. IN_PROGRESS is a transient concurrent
	// double-tap — retryable after the advised delay.
	CodeIdempotencyKeyRequired = Register("IDEMPOTENCY_KEY_REQUIRED", 400, false, "This mutating endpoint requires an Idempotency-Key header.")
	CodeIdempotencyKeyReuse    = Register("IDEMPOTENCY_KEY_REUSED", 409, false, "The Idempotency-Key was already used with a different request body.")
	CodeIdempotencyInProgress  = Register("IDEMPOTENCY_IN_PROGRESS", 409, true, "A request with this Idempotency-Key is already in progress; retry after the advised delay.")
)
