// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package resilience_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration/resilience"
)

// waitTimes runs n sequential Waits and returns when each one returned,
// relative to the start.
func waitTimes(t *testing.T, l resilience.Limiter, n int) []time.Duration {
	t.Helper()
	start := time.Now()
	var out []time.Duration
	for range n {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		out = append(out, time.Since(start))
	}
	return out
}

func TestTokenBucket_Burst(t *testing.T) {
	const s = time.Second
	tests := []struct {
		name  string
		rate  float64
		burst int
		idle  time.Duration // wait before starting
		want  []time.Duration
	}{
		{name: "burst then rate", rate: 1, burst: 3, want: []time.Duration{0, 0, 0, s, 2 * s}},
		{name: "burst 1", rate: 2, burst: 1, want: []time.Duration{0, s / 2, s, 3 * s / 2}},
		{name: "burst below 1 is 1", rate: 1, burst: 0, want: []time.Duration{0, s}},
		{name: "refill capped at burst", rate: 1, burst: 2, idle: time.Minute, want: []time.Duration{0, 0, s}},
		{name: "unlimited", rate: 0, burst: 1, want: []time.Duration{0, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := resilience.NewTokenBucket(tt.rate, tt.burst)
				if tt.idle > 0 {
					l.Wait(t.Context()) // start the clock, then drain below full
					time.Sleep(tt.idle)
				}
				if got := waitTimes(t, l, len(tt.want)); !slices.Equal(got, tt.want) {
					t.Errorf("waits returned at %v, want %v", got, tt.want)
				}
			})
		})
	}
}

func TestTokenBucket_CancelReturnsToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := resilience.NewTokenBucket(1, 1)
		l.Wait(t.Context()) // bucket now empty

		cause := errors.New("caller gave up")
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(100*time.Millisecond, func() { cancel(cause) })
		if err := l.Wait(ctx); !errors.Is(err, cause) {
			t.Fatalf("Wait() = %v, want the cancellation cause", err)
		}

		// The abandoned reservation went back: the next waiter is served
		// at 1s, not 2s.
		start := time.Now()
		if err := l.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 900*time.Millisecond {
			t.Errorf("next Wait took %v, want 900ms", got)
		}
	})
}

func TestTokenBucket_DeadlineTooSoon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := resilience.NewTokenBucket(1, 1)
		l.Wait(t.Context())

		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := l.Wait(ctx); !errors.Is(err, resilience.ErrRateLimited) {
			t.Fatalf("Wait() = %v, want ErrRateLimited", err)
		}
		if time.Since(start) != 0 {
			t.Errorf("waited %v before failing, want 0", time.Since(start))
		}
		if got := waitTimes(t, l, 1); got[0] != time.Second {
			t.Errorf("next Wait returned after %v, want 1s (token returned)", got[0])
		}
	})
}

func TestTokenBucket_DoneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := resilience.NewTokenBucket(0, 1).Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait(done ctx) = %v, want context.Canceled even when unlimited", err)
	}
}

func TestTokenBucket_FIFOUnderContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := resilience.NewTokenBucket(10, 1)
		var mu sync.Mutex
		var times []time.Duration
		start := time.Now()
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if err := l.Wait(t.Context()); err != nil {
					t.Error(err)
				}
				mu.Lock()
				times = append(times, time.Since(start))
				mu.Unlock()
			})
		}
		wg.Wait()
		slices.Sort(times)
		for i, d := range times { // one every 100ms, none early, none lost
			if want := time.Duration(i) * 100 * time.Millisecond; d != want {
				t.Fatalf("waiter %d served at %v, want %v", i, d, want)
			}
		}
	})
}

func TestTokenBucket_ConcurrentRace(t *testing.T) {
	l := resilience.NewTokenBucket(1e12, 100) // every wait rounds to 0: no real sleeping
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 20 {
				if err := l.Wait(t.Context()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}
