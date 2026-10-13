// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
	"uuid"
)

// AttemptInfo describes one attempt made by [Retry].
type AttemptInfo struct {
	Attempt    int // 1-based
	StatusCode int // 0 when the attempt failed without a response
	Err        error
	Duration   time.Duration
	Retry      bool          // another attempt follows
	Delay      time.Duration // the wait before it
}

type attemptHooksKey struct{}

// ObserveAttempts returns a copy of ctx that makes Retry call fn after
// every attempt of a call made with it. Hooks registered further out in
// the chain stay in place, so the outer middleware (Tracing, Metrics,
// Logging) all see each attempt. fn runs on the request's goroutine and
// must not block.
func ObserveAttempts(ctx context.Context, fn func(AttemptInfo)) context.Context {
	hooks, _ := ctx.Value(attemptHooksKey{}).([]func(AttemptInfo))
	return context.WithValue(ctx, attemptHooksKey{}, append(slices.Clip(hooks), fn))
}

// notifyAttempt runs the hooks registered on ctx.
func notifyAttempt(ctx context.Context, info AttemptInfo) {
	hooks, _ := ctx.Value(attemptHooksKey{}).([]func(AttemptInfo))
	for _, fn := range hooks {
		fn(info)
	}
}

type routeKey struct{}

// WithRoute returns a copy of ctx naming the route template of calls made
// with it, such as "/invoices/{id}". Logs and metrics use it instead of the
// raw path, which may carry IDs and would explode metric cardinality.
func WithRoute(ctx context.Context, route string) context.Context {
	return context.WithValue(ctx, routeKey{}, route)
}

// Route returns the route template for req: the one set with [WithRoute],
// or else req's path with ID-like segments replaced by "{id}". It never
// includes the query string.
func Route(req *http.Request) string {
	if r, ok := req.Context().Value(routeKey{}).(string); ok && r != "" {
		return r
	}
	return templatePath(req.URL.Path)
}

// templatePath replaces the segments of p that look like identifiers.
func templatePath(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if looksLikeID(s) {
			segs[i] = "{id}"
		}
	}
	return strings.Join(segs, "/")
}

// looksLikeID reports whether a path segment is probably a value rather
// than part of the route: a number, a UUID, or a long token with digits.
func looksLikeID(s string) bool {
	if s == "" {
		return false
	}
	if _, err := uuid.Parse(s); err == nil {
		return true
	}
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits == len(s) || (len(s) >= 16 && digits > 0)
}
