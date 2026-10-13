// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

// Package transport holds the [http.RoundTripper] middleware of the
// integration layer and [Chain], which assembles them.
//
// Every middleware follows the RoundTripper contract: it clones the request
// before changing anything on it, and it closes the body of any response it
// does not hand back to its caller.
package transport

import (
	"net/http"
	"slices"
)

// Middleware wraps a RoundTripper with extra behavior.
type Middleware func(http.RoundTripper) http.RoundTripper

// RoundTripperFunc adapts an ordinary function to [http.RoundTripper].
type RoundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f(req).
func (f RoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Chain wraps base in mws so that mws[0] is outermost: it sees the request
// first and the response last. Each middleware is applied exactly once, when
// Chain runs, so state a middleware builds (a limiter, a breaker) is shared
// by every request. Nil middleware are skipped. base must not be nil.
func Chain(base http.RoundTripper, mws ...Middleware) http.RoundTripper {
	rt := base
	for _, mw := range slices.Backward(mws) {
		if mw != nil {
			rt = mw(rt)
		}
	}
	return rt
}
