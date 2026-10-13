// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"net/http"
)

// Tracer starts a span per logical call and propagates it. Implement it
// over OpenTelemetry (or anything else) outside the core packages.
type Tracer interface {
	// Start begins a span for req. The returned func ends it with the
	// call's final response or error.
	Start(ctx context.Context, req *http.Request) (context.Context, func(resp *http.Response, err error))
	// Inject writes the propagation headers for the span in ctx into h.
	Inject(ctx context.Context, h http.Header)
}

// AttemptTracer is optionally implemented by a [Tracer] to record each
// attempt within the call's span, typically as a span event.
type AttemptTracer interface {
	TraceAttempt(ctx context.Context, a AttemptInfo)
}

// Tracing returns middleware that wraps each logical call in one span,
// injects its propagation headers into the outgoing request, and ends it
// with the final response or error. It is the outermost middleware, so
// the request ID and logs downstream run inside the span's context.
func Tracing(t Tracer) Middleware {
	at, recordsAttempts := t.(AttemptTracer)
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			spanCtx, finish := t.Start(req.Context(), req)
			ctx := spanCtx
			if recordsAttempts {
				ctx = ObserveAttempts(ctx, func(a AttemptInfo) { at.TraceAttempt(spanCtx, a) })
			}
			treq := req.Clone(ctx)
			t.Inject(spanCtx, treq.Header)

			resp, err := next.RoundTrip(treq)
			finish(resp, err)
			return resp, err
		})
	}
}
