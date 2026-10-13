// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package resilience

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Limiter paces calls.
type Limiter interface {
	// Wait blocks until a call may proceed or ctx is done.
	Wait(ctx context.Context) error
}

// TokenBucket is a token-bucket [Limiter]: it starts full with burst
// tokens and refills at rate per second up to burst. Each Wait takes one
// token, reserving a future one if none is left, so waiters are served in
// the order they arrived. It is safe for concurrent use.
type TokenBucket struct {
	rate  float64 // tokens per second; <= 0 means unlimited
	burst float64

	mu     sync.Mutex
	tokens float64 // may be negative: tokens already promised to waiters
	last   time.Time
}

// NewTokenBucket returns a limiter allowing rate calls per second with
// bursts of up to burst (minimum 1). A rate <= 0 never waits.
func NewTokenBucket(rate float64, burst int) *TokenBucket {
	b := float64(max(burst, 1))
	return &TokenBucket{rate: rate, burst: b, tokens: b}
}

// Wait takes a token, waiting for one if needed. It fails at once with an
// error wrapping ErrRateLimited if ctx's deadline would pass before the
// token is available, and with context.Cause(ctx) if ctx ends while
// waiting; either way the reserved token is returned to the bucket.
func (l *TokenBucket) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if l.rate <= 0 || math.IsInf(l.rate, 1) {
		return nil
	}

	l.mu.Lock()
	now := time.Now()
	l.refill(now)
	l.tokens--
	var wait time.Duration
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	l.mu.Unlock()

	if wait == 0 {
		return nil
	}
	if deadline, ok := ctx.Deadline(); ok && deadline.Sub(now) < wait {
		l.giveBack()
		return fmt.Errorf("%w: next slot in %v, after the deadline", ErrRateLimited, wait)
	}
	if err := Sleep(ctx, wait); err != nil {
		l.giveBack()
		return err
	}
	return nil
}

// refill adds the tokens earned since the last refill. l.mu must be held.
func (l *TokenBucket) refill(now time.Time) {
	if !l.last.IsZero() {
		l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	}
	l.last = now
}

// giveBack returns an unused reservation.
func (l *TokenBucket) giveBack() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill(time.Now())
	l.tokens = min(l.burst, l.tokens+1)
}
