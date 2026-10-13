// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"net/http"
	"uuid"
)

// Correlation headers.
const (
	HeaderRequestID      = "X-Request-ID"
	HeaderIdempotencyKey = "Idempotency-Key"
)

type (
	requestIDKey  struct{}
	idempotentKey struct{}
)

// WithRequestID returns a copy of ctx carrying id. [RequestID] sends it as
// X-Request-ID, so an ID received from an inbound request can be
// propagated to outbound calls.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the ID stored by [WithRequestID], if any.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok && id != ""
}

// Idempotent returns a copy of ctx that opts a POST or PATCH made with it
// into an Idempotency-Key, which also makes it retryable.
func Idempotent(ctx context.Context) context.Context {
	return context.WithValue(ctx, idempotentKey{}, true)
}

func isIdempotent(ctx context.Context) bool {
	v, _ := ctx.Value(idempotentKey{}).(bool)
	return v
}

// RequestIDOption configures [RequestID].
type RequestIDOption func(*requestIDOptions)

type requestIDOptions struct {
	allIdempotent bool
}

// IdempotencyKeys gives every POST and PATCH an Idempotency-Key, not only
// those whose context was marked with [Idempotent].
func IdempotencyKeys() RequestIDOption {
	return func(o *requestIDOptions) { o.allIdempotent = true }
}

// RequestID returns middleware that gives each logical call an
// X-Request-ID (kept if the request has one, else taken from the context,
// else a new UUID) and stores it on the request context. For an opted-in
// POST or PATCH without an Idempotency-Key it adds a new one. It sits
// outside Retry, so every attempt carries the same ID and key.
func RequestID(opts ...RequestIDOption) Middleware {
	var o requestIDOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			ctx := req.Context()
			id := req.Header.Get(HeaderRequestID)
			if id == "" {
				id, _ = RequestIDFromContext(ctx)
			}
			if id == "" {
				id = uuid.New().String()
			}
			if cur, _ := RequestIDFromContext(ctx); cur != id {
				ctx = WithRequestID(ctx, id)
			}

			areq := req.Clone(ctx)
			areq.Header.Set(HeaderRequestID, id)
			if (req.Method == http.MethodPost || req.Method == http.MethodPatch) &&
				(o.allIdempotent || isIdempotent(ctx)) && req.Header.Get(HeaderIdempotencyKey) == "" {
				areq.Header.Set(HeaderIdempotencyKey, uuid.New().String())
			}
			return next.RoundTrip(areq)
		})
	}
}
