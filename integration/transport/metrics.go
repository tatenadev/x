// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

// MetricsRecorder receives one record per logical call. Adapt it to
// Prometheus, OpenTelemetry metrics or anything else outside the core
// packages.
type MetricsRecorder interface {
	RecordRequest(ctx context.Context, info RequestInfo)
}

// RequestInfo describes one logical call, all attempts included.
type RequestInfo struct {
	Service    string
	Method     string
	Route      string // templated path, never the raw URL (cardinality)
	StatusCode int    // 0 on transport error
	Attempts   int
	Duration   time.Duration
	Err        error
}

// Metrics returns middleware that reports every logical call to rec. It
// sits outside Retry: one record per call, with the attempt count.
func Metrics(rec MetricsRecorder, service string) Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			var attempts atomic.Int32
			ctx := ObserveAttempts(req.Context(), func(AttemptInfo) { attempts.Add(1) })

			start := time.Now()
			resp, err := next.RoundTrip(req.WithContext(ctx))
			rec.RecordRequest(req.Context(), RequestInfo{
				Service:    service,
				Method:     req.Method,
				Route:      Route(req),
				StatusCode: statusOf(resp),
				Attempts:   max(int(attempts.Load()), 1),
				Duration:   time.Since(start),
				Err:        err,
			})
			return resp, err
		})
	}
}
