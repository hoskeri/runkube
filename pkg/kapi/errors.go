package kapi

import "fmt"

// ── Error handling ─────────────────────────────────────────

// HTTPStatusError may be implemented by handler errors to control
// the HTTP status code returned in the response body (as Status).
// If an error returned by a handler does not implement this interface,
// the library uses 500 Internal Server Error.
type HTTPStatusError interface {
	HTTPStatus() int32
}

// statusError is the internal implementation of HTTPStatusError.
type statusError struct {
	code    int32
	message string
}

func (e *statusError) Error() string     { return e.message }
func (e *statusError) HTTPStatus() int32 { return e.code }

// Convenience constructors. Each returns an error that implements
// HTTPStatusError with the standard Kubernetes semantic code.

// NotFound returns an error that maps to 404 Not Found.
func NotFound(name string) error {
	return &statusError{code: 404, message: fmt.Sprintf("%q not found", name)}
}

// AlreadyExists returns an error that maps to 409 Conflict.
func AlreadyExists(name string) error {
	return &statusError{code: 409, message: fmt.Sprintf("%q already exists", name)}
}

// Conflict returns an error that maps to 409 Conflict.
func Conflict(name string) error {
	return &statusError{code: 409, message: fmt.Sprintf("conflict: %s", name)}
}

// Forbidden returns an error that maps to 403 Forbidden.
func Forbidden(reason string) error {
	return &statusError{code: 403, message: reason}
}

// BadRequest returns an error that maps to 400 Bad Request.
func BadRequest(msg string) error {
	return &statusError{code: 400, message: msg}
}

// InternalError returns an error that maps to 500 Internal Server Error.
func InternalError(msg string) error {
	return &statusError{code: 500, message: msg}
}
