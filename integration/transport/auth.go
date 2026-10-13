// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"fmt"
	"net/http"

	"github.com/tatenadev/x/integration/auth"
)

// AuthOption configures [Auth].
type AuthOption func(*authOptions)

type authOptions struct {
	overwrite bool
}

// OverwriteAuth makes [Auth] replace a credential header the request
// already carries. By default such a request is sent unchanged.
func OverwriteAuth() AuthOption {
	return func(o *authOptions) { o.overwrite = true }
}

// Auth returns middleware that sets the credential header from src on
// every attempt, so a retry picks up a refreshed token. It sits inside
// Retry in the chain.
//
// On a 401 it calls src's Invalidate (when src is an [auth.Invalidator])
// once, and only if the rejected token is still the current one: a late
// 401 for an older token must not discard a newer token. The 401 response
// itself is returned unchanged for Retry to act on.
func Auth(src auth.TokenSource, opts ...AuthOption) Middleware {
	var o authOptions
	for _, opt := range opts {
		opt(&o)
	}
	inv, canInvalidate := src.(auth.Invalidator)

	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			tok, err := src.Token(req.Context())
			if err != nil {
				closeRequestBody(req)
				return nil, fmt.Errorf("%w: %w", auth.ErrTokenUnavailable, err)
			}
			name, value := tok.Header()
			if !o.overwrite && req.Header.Get(name) != "" {
				return next.RoundTrip(req)
			}

			areq := req.Clone(req.Context())
			areq.Header.Set(name, value)
			resp, err := next.RoundTrip(areq)
			if err == nil && resp.StatusCode == http.StatusUnauthorized && canInvalidate {
				if cur, err := src.Token(req.Context()); err == nil {
					if _, curValue := cur.Header(); curValue == value {
						inv.Invalidate()
					}
				}
			}
			return resp, err
		})
	}
}

// closeRequestBody closes req's body. A RoundTripper must close the
// request body even when it fails before sending.
func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}
