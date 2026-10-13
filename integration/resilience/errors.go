// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

// Package resilience holds protocol-agnostic building blocks of the
// integration layer: backoff, Retry-After parsing, and (later) the circuit
// breaker and rate limiter. It depends on nothing else in the layer, so the
// sentinel errors live here and package integration re-exports them.
package resilience

import "errors"

var (
	// ErrCircuitOpen is returned while the circuit breaker rejects calls.
	// It is terminal: retrying inside the same call cannot succeed.
	ErrCircuitOpen = errors.New("integration: circuit open")
	// ErrRateLimited is returned when the local rate limiter refuses a call.
	ErrRateLimited = errors.New("integration: rate limited")
	// ErrAttemptTimeout is the context cause of a per-attempt timeout. It
	// tells a retryable attempt timeout apart from caller cancellation.
	ErrAttemptTimeout = errors.New("integration: attempt timed out")
)
