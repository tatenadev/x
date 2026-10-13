// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/resilience"
)

// CircuitBreaker returns middleware that consults b before each attempt
// and records the outcome. Only upstream failures count against it:
// transport errors (including attempt timeouts) and 5xx responses. A 4xx
// is the upstream working as intended, so it counts as a success. Caller
// cancellation and token-source failures say nothing about the upstream
// and are not recorded.
//
// A rejected attempt fails with an error wrapping
// resilience.ErrCircuitOpen, which Retry treats as terminal.
func CircuitBreaker(b resilience.Breaker) Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if err := b.Allow(); err != nil {
				closeRequestBody(req)
				return nil, fmt.Errorf("breaker: %w", err)
			}
			resp, err := next.RoundTrip(req)
			if err != nil {
				if upstreamFailure(req.Context(), err) {
					b.Record(false)
				}
				return nil, err
			}
			b.Record(resp.StatusCode < http.StatusInternalServerError)
			return resp, nil
		})
	}
}

// upstreamFailure reports whether a transport error reflects on the
// upstream rather than on the caller or on the credentials.
func upstreamFailure(ctx context.Context, err error) bool {
	if errors.Is(err, auth.ErrTokenUnavailable) {
		return false
	}
	if ctx.Err() != nil {
		// Only our own attempt timeout blames the upstream.
		return errors.Is(context.Cause(ctx), resilience.ErrAttemptTimeout)
	}
	return true
}
