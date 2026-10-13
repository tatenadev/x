// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"fmt"
	"net/http"

	"github.com/tatenadev/x/integration/resilience"
)

// RateLimit returns middleware that waits on l before each attempt. It sits
// inside Retry, so every attempt spends a token. If the wait fails (the
// request's context ends, or the deadline would pass first) the request is
// not sent and its body is closed.
func RateLimit(l resilience.Limiter) Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if err := l.Wait(req.Context()); err != nil {
				closeRequestBody(req)
				return nil, fmt.Errorf("ratelimit: %w", err)
			}
			return next.RoundTrip(req)
		})
	}
}
