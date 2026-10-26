package ks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
)

// Framework error codes. Action-declared codes are additional and always lowercase.
const (
	CodeNotFound        = "not_found"
	CodeInvalid         = "invalid"
	CodeForbidden       = "forbidden"
	CodeUnauthenticated = "unauthenticated"
	CodeConflict        = "conflict"
	CodeInvariant       = "invariant_violation"
	CodeRateLimited     = "rate_limited"
	CodeBudget          = "query_budget_exceeded"
	CodeTimeout         = "timeout"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
	CodeNotImplemented  = "not_implemented"
)

// Error is the single error type crossing the action boundary. Message is safe to
// show a caller; Internal carries the real cause for logs and traces only.
type Error struct {
	Code     string
	Message  string
	Fields   map[string]string
	Status   int
	Internal error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Internal }

// NewError builds an error with an explicit code and HTTP status.
func NewError(code string, status int, msg string) *Error {
	return &Error{Code: code, Status: status, Message: msg}
}

// NotFound reports a missing row, or one the actor is not allowed to see (§8.1).
// Existence is never revealed, so both cases use this single error.
func NotFound() *Error {
	return &Error{Code: CodeNotFound, Status: http.StatusNotFound, Message: "Resource not found."}
}

// Invalid reports a failed input validation, naming the field.
func Invalid(field, message string) *Error {
	return &Error{
		Code:    CodeInvalid,
		Status:  http.StatusBadRequest,
		Message: "Invalid input.",
		Fields:  map[string]string{field: message},
	}
}

// InvalidMulti reports several failed field checks.
func InvalidMulti(fields map[string]string) *Error {
	return &Error{Code: CodeInvalid, Status: http.StatusBadRequest, Message: "Invalid input.", Fields: fields}
}

// Forbidden reports a rejected Requires or write rule.
func Forbidden(why string) *Error {
	if why == "" {
		why = "Not allowed."
	}
	return &Error{Code: CodeForbidden, Status: http.StatusForbidden, Message: why}
}

// Unauthenticated reports a missing or expired credential.
func Unauthenticated() *Error {
	return &Error{Code: CodeUnauthenticated, Status: http.StatusUnauthorized, Message: "Authentication required."}
}

// Conflict reports a stale optimistic-concurrency version (§8.1).
func Conflict() *Error {
	return &Error{Code: CodeConflict, Status: http.StatusConflict, Message: "The record was changed by someone else; retry."}
}

// Invariant reports a runtime invariant check that refused a write (§5.5).
func Invariant(name string) *Error {
	return &Error{
		Code:     CodeInvariant,
		Status:   http.StatusUnprocessableEntity,
		Message:  fmt.Sprintf("Invariant %q does not hold.", name),
		Internal: errors.New("invariant: " + name),
	}
}

// RateLimited reports an exceeded request limit.
func RateLimited(retryAfter int) *Error {
	return &Error{Code: CodeRateLimited, Status: http.StatusTooManyRequests, Message: "Too many requests."}
}

// BudgetExceeded reports an action going over its query or row budget.
func BudgetExceeded(what string, limit int) *Error {
	return &Error{
		Code:     CodeBudget,
		Status:   http.StatusUnprocessableEntity,
		Message:  fmt.Sprintf("%s budget of %d exceeded.", what, limit),
		Internal: fmt.Errorf("budget: %s > %d", what, limit),
	}
}

// NotImplemented is returned by action stubs that ks gen wrote but nobody filled in.
func NotImplemented() *Error {
	return &Error{Code: CodeNotImplemented, Status: http.StatusInternalServerError, Message: "Action not implemented."}
}

// InternalError wraps an unexpected failure; its message is never shown to callers.
func InternalError(err error) *Error {
	return &Error{Code: CodeInternal, Status: http.StatusInternalServerError, Message: "Internal error.", Internal: err}
}

// Timeout reports a statement or total deadline overrun.
func Timeout(what string) *Error {
	return &Error{Code: CodeTimeout, Status: http.StatusGatewayTimeout, Message: "The operation timed out.", Internal: errors.New("timeout: " + what)}
}

// Unavailable reports shedding or pool exhaustion.
func Unavailable(why string) *Error {
	return &Error{Code: CodeUnavailable, Status: http.StatusServiceUnavailable, Message: why}
}

// ActionError builds a declared action-specific error (gen.ErrNotDraft calls this).
// Declared action errors surface as 422 (§7.3).
func ActionError(code, message string) *Error {
	return &Error{Code: code, Status: http.StatusUnprocessableEntity, Message: message}
}

// AsError extracts a *Error, wrapping anything else as an internal error.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout(err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Code: CodeInternal, Status: http.StatusLocked, Message: "Request cancelled."}
	}
	return InternalError(err)
}

// Status maps an error to its HTTP status.
func Status(err error) int {
	e := AsError(err)
	if e == nil {
		return http.StatusOK
	}
	if e.Status != 0 {
		return e.Status
	}
	return http.StatusInternalServerError
}

// CodesIn returns the framework codes in deterministic order, for docs and clients.
func CodesIn() []string {
	out := []string{
		CodeNotFound, CodeInvalid, CodeForbidden, CodeUnauthenticated, CodeConflict,
		CodeInvariant, CodeRateLimited, CodeBudget, CodeTimeout, CodeUnavailable,
		CodeInternal, CodeNotImplemented,
	}
	sort.Strings(out)
	return out
}
