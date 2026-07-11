// Package errors is the platform's stable error-code registry and the 02 §2
// error envelope, with HTTP-status mapping helpers.
//
// A `code` is a stable, machine-readable UPPER_SNAKE string registered here
// once; `message` is human-only and may change freely. Every non-2xx response
// from every service/BFF serialises as the same envelope:
//
//	{"error":{"code","message","details":[…],"trace_id","retryable"}}
//
// The registry is the single source of truth for (HTTP status, retryable,
// default message) per code — so the whole fleet maps a code to a status the
// same way (02 §2 "HTTP mapping").
package errors

import (
	stderrors "errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// Detail is one typed line item in an error's details[] list. Using a list of
// typed items (never scalar fields) is the 02 §5 extensibility rule.
type Detail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// spec is the registered metadata for a code.
type spec struct {
	status     int
	retryable  bool
	defaultMsg string
}

var (
	mu       sync.RWMutex
	registry = map[string]spec{}
)

// Register adds (or overrides) a code in the registry. Codes MUST be
// UPPER_SNAKE; Register panics otherwise so a bad code fails at init time, not
// in production. It returns the code for convenient `var X = Register(...)`.
func Register(code string, status int, retryable bool, defaultMsg string) string {
	if !isUpperSnake(code) {
		panic(fmt.Sprintf("errors: code %q is not UPPER_SNAKE", code))
	}
	if status < 100 || status > 599 {
		panic(fmt.Sprintf("errors: code %q has invalid HTTP status %d", code, status))
	}
	mu.Lock()
	defer mu.Unlock()
	registry[code] = spec{status: status, retryable: retryable, defaultMsg: defaultMsg}
	return code
}

// Registered reports whether a code exists in the registry.
func Registered(code string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := registry[code]
	return ok
}

// Codes returns every registered code, sorted — used by the code-registry test
// and by docs generation.
func Codes() []string {
	mu.RLock()
	out := make([]string, 0, len(registry))
	for c := range registry {
		out = append(out, c)
	}
	mu.RUnlock()
	sort.Strings(out)
	return out
}

// HTTPStatus returns the registered HTTP status for a code (500 if unknown).
func HTTPStatus(code string) int {
	mu.RLock()
	defer mu.RUnlock()
	if s, ok := registry[code]; ok {
		return s.status
	}
	return http.StatusInternalServerError
}

// Retryable returns whether clients may safely retry a code (false if unknown).
func Retryable(code string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return registry[code].retryable
}

// DefaultMessage returns the registered default human message for a code.
func DefaultMessage(code string) string {
	mu.RLock()
	defer mu.RUnlock()
	return registry[code].defaultMsg
}

// Error is a platform error carrying a registered code, a human message, typed
// details, and an optional wrapped cause. It implements error and errors.Is/As.
type Error struct {
	Code    string
	Message string
	Details []Detail
	cause   error
}

// New builds an Error from a code, defaulting the message to the registered
// default when msg is empty.
func New(code string, msg string, details ...Detail) *Error {
	if msg == "" {
		msg = DefaultMessage(code)
	}
	return &Error{Code: code, Message: msg, Details: details}
}

// Wrap builds an Error that wraps cause (for %w / errors.Unwrap chains).
func Wrap(code string, cause error, msg string, details ...Detail) *Error {
	e := New(code, msg, details...)
	e.cause = cause
	return e
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the wrapped cause.
func (e *Error) Unwrap() error { return e.cause }

// Is matches by code, so `errors.Is(err, errors.New(CodeConflict, ""))` works.
func (e *Error) Is(target error) bool {
	var t *Error
	if stderrors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// Status returns the HTTP status for this error's code.
func (e *Error) Status() int { return HTTPStatus(e.Code) }

// RetryableFlag returns the retryable flag for this error's code.
func (e *Error) RetryableFlag() bool { return Retryable(e.Code) }

// Envelope is the 02 §2 wire shape.
type Envelope struct {
	Error EnvelopeBody `json:"error"`
}

// EnvelopeBody is the inner error object.
type EnvelopeBody struct {
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Details   []Detail `json:"details"`
	TraceID   string   `json:"trace_id"`
	Retryable bool     `json:"retryable"`
}

// ToEnvelope renders any error as the 02 §2 envelope. A *Error uses its
// registered mapping; any other error becomes INTERNAL (500, non-retryable) so
// no handler can ever leak a non-conforming body.
func ToEnvelope(err error, traceID string) (int, Envelope) {
	var pe *Error
	if !stderrors.As(err, &pe) {
		pe = New(CodeInternal, "")
	}
	details := pe.Details
	if details == nil {
		details = []Detail{}
	}
	return pe.Status(), Envelope{Error: EnvelopeBody{
		Code:      pe.Code,
		Message:   pe.Message,
		Details:   details,
		TraceID:   traceID,
		Retryable: pe.RetryableFlag(),
	}}
}

func isUpperSnake(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}
