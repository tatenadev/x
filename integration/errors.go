// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tatenadev/x/integration/resilience"
)

// Sentinel errors. Match them with [errors.Is]. The resilience ones are
// shared with the transports, which cannot import this package.
var (
	// ErrCircuitOpen is returned while the circuit breaker rejects calls.
	// It is terminal: retrying inside the same call cannot succeed.
	ErrCircuitOpen = resilience.ErrCircuitOpen
	// ErrRateLimited is returned when the local rate limiter refuses a call.
	ErrRateLimited = resilience.ErrRateLimited
	// ErrAttemptTimeout is the context cause of a per-attempt timeout. It
	// distinguishes a retryable attempt timeout from caller cancellation.
	ErrAttemptTimeout = resilience.ErrAttemptTimeout
	// ErrInvalidConfig wraps every [Config] validation failure.
	ErrInvalidConfig = errors.New("integration: invalid config")
)

// maxErrorBody caps how much of a non-2xx response body is kept in
// [Error.Body] for diagnostics.
const maxErrorBody = 4 << 10

// Error describes a failed call: a transport error (StatusCode 0) or a
// non-2xx response. Inspect it with errors.AsType[*integration.Error].
//
// Error has pointer receivers only, so only *Error implements error. This
// keeps errors.AsType and %w wrapping unambiguous.
type Error struct {
	Service    string
	Method     string
	URL        string // redacted: no password, no secret query values
	StatusCode int    // 0 if no response was received
	Retryable  bool   // whether a later attempt of the same call may succeed
	RequestID  string // X-Request-ID sent with the request, if any
	Body       []byte // first 4 KiB of a non-2xx body, for diagnostics
	Err        error  // underlying transport error; nil for status errors
}

// Error formats e without its Body, which may be large or sensitive.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("integration: ")
	if e.Service != "" {
		b.WriteString(e.Service)
		b.WriteString(": ")
	}
	b.WriteString(e.Method)
	b.WriteByte(' ')
	b.WriteString(e.URL)
	if e.StatusCode != 0 {
		b.WriteString(": ")
		b.WriteString(strconv.Itoa(e.StatusCode))
		if text := http.StatusText(e.StatusCode); text != "" {
			b.WriteByte(' ')
			b.WriteString(text)
		}
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying transport error, if any.
func (e *Error) Unwrap() error { return e.Err }

// IsRetryable reports whether err is, or wraps, an [*Error] marked
// retryable.
func IsRetryable(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Retryable
	}
	return false
}

// retryableStatus reports whether a response status is worth retrying:
// 429 and the gateway-class 5xx, plus a plain 500 when retry500 is set.
func retryableStatus(code int, retry500 bool) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	case http.StatusInternalServerError:
		return retry500
	}
	return false
}
