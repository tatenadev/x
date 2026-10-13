// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package resilience

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Backoff computes capped exponential backoff with full jitter: the wait
// before retry n is uniform in [0, min(max, base·2^(n-1))). A max <= 0
// means uncapped. It is safe for concurrent use.
type Backoff struct {
	base, max time.Duration

	mu   sync.Mutex // guards rand, which is not safe for concurrent use
	rand *rand.Rand // nil: the global source
}

// NewBackoff returns a Backoff. r supplies the jitter; pass a seeded
// *rand.Rand for deterministic delays in tests, or nil for the global
// source.
func NewBackoff(base, max time.Duration, r *rand.Rand) *Backoff {
	return &Backoff{base: base, max: max, rand: r}
}

// Max returns the cap on any single delay. A nil Backoff has none.
func (b *Backoff) Max() time.Duration {
	if b == nil {
		return 0
	}
	return b.max
}

// Ceiling returns the un-jittered upper bound for retry n (1 = first
// retry). A nil Backoff never waits.
func (b *Backoff) Ceiling(retry int) time.Duration {
	if b == nil || retry < 1 || b.base <= 0 {
		return 0
	}
	const longest = time.Duration(1<<63 - 1)
	d := b.base
	for range retry - 1 {
		if b.max > 0 && d >= b.max/2 {
			return b.max
		}
		if d > longest/2 { // doubling would overflow
			return longest
		}
		d *= 2
	}
	if b.max > 0 {
		return min(d, b.max)
	}
	return d
}

// Delay returns the jittered wait before retry n (1 = first retry).
func (b *Backoff) Delay(retry int) time.Duration {
	ceil := b.Ceiling(retry)
	if ceil <= 0 {
		return 0
	}
	if b.rand == nil {
		return rand.N(ceil)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rand.N(ceil)
}

// ParseRetryAfter parses a Retry-After header value (RFC 9110 §10.2.3):
// either delay-seconds or an HTTP-date, measured from now. It reports false
// for an absent, malformed, or negative value. A date in the past is a
// zero delay.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs > int64(time.Duration(1<<63-1)/time.Second) {
			return time.Duration(1<<63 - 1), true // absurd, but not negative
		}
		return time.Duration(secs) * time.Second, true
	}
	when, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return max(when.Sub(now), 0), true
}

// Sleep waits for d or until ctx is done, whichever comes first. It
// returns context.Cause(ctx) if ctx ended first.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}
