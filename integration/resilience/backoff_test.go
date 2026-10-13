// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package resilience_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration/resilience"
)

func TestBackoff_Ceiling(t *testing.T) {
	const ms = time.Millisecond
	tests := []struct {
		name      string
		base, max time.Duration
		retry     int
		want      time.Duration
	}{
		{name: "first", base: 100 * ms, max: 5 * time.Second, retry: 1, want: 100 * ms},
		{name: "doubles", base: 100 * ms, max: 5 * time.Second, retry: 4, want: 800 * ms},
		{name: "capped", base: 100 * ms, max: 5 * time.Second, retry: 7, want: 5 * time.Second},
		{name: "odd cap", base: 100 * ms, max: 250 * ms, retry: 2, want: 200 * ms},
		{name: "odd cap reached", base: 100 * ms, max: 250 * ms, retry: 3, want: 250 * ms},
		{name: "huge retry stays capped", base: 100 * ms, max: 5 * time.Second, retry: 10_000, want: 5 * time.Second},
		{name: "uncapped huge retry does not overflow", base: 100 * ms, retry: 10_000, want: time.Duration(1<<63 - 1)},
		{name: "retry 0", base: 100 * ms, max: time.Second, retry: 0, want: 0},
		{name: "zero base", base: 0, max: time.Second, retry: 3, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resilience.NewBackoff(tt.base, tt.max, nil).Ceiling(tt.retry); got != tt.want {
				t.Errorf("Ceiling(%d) = %v, want %v", tt.retry, got, tt.want)
			}
		})
	}
}

func TestBackoff_Nil(t *testing.T) {
	var b *resilience.Backoff
	if b.Delay(3) != 0 || b.Ceiling(3) != 0 || b.Max() != 0 {
		t.Error("nil Backoff must never wait")
	}
}

func delays(b *resilience.Backoff, n int) []time.Duration {
	var out []time.Duration
	for retry := 1; retry <= n; retry++ {
		out = append(out, b.Delay(retry))
	}
	return out
}

func TestBackoff_FullJitterDeterministic(t *testing.T) {
	newB := func(seed uint64) *resilience.Backoff {
		return resilience.NewBackoff(100*time.Millisecond, 5*time.Second, rand.New(rand.NewPCG(seed, 0)))
	}
	a, b := delays(newB(1), 10), delays(newB(1), 10)
	if !slices.Equal(a, b) {
		t.Errorf("same seed, different delays:\n%v\n%v", a, b)
	}
	if c := delays(newB(2), 10); slices.Equal(a, c) {
		t.Errorf("different seeds, same delays: %v", a)
	}
	ref := newB(1)
	for i, d := range a {
		if ceil := ref.Ceiling(i + 1); d < 0 || d >= ceil {
			t.Errorf("Delay(%d) = %v, want in [0, %v)", i+1, d, ceil)
		}
	}
}

func TestBackoff_ConcurrentRace(t *testing.T) {
	b := resilience.NewBackoff(time.Millisecond, time.Second, rand.New(rand.NewPCG(1, 2)))
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { delays(b, 20) })
	}
	wg.Wait()
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{in: "120", want: 2 * time.Minute, wantOK: true},
		{in: " 5 ", want: 5 * time.Second, wantOK: true},
		{in: "0", want: 0, wantOK: true},
		{in: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second, wantOK: true},
		{in: now.Add(-time.Hour).Format(http.TimeFormat), want: 0, wantOK: true},
		{in: "Mon, 13 Oct 2026 12:00:30 GMT", want: 30 * time.Second, wantOK: true},
		{in: "99999999999999999999", wantOK: false}, // overflows int64
		{in: "9999999999999", want: time.Duration(1<<63 - 1), wantOK: true},
		{in: "", wantOK: false},
		{in: "-1", wantOK: false},
		{in: "1.5", wantOK: false},
		{in: "soon", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := resilience.ParseRetryAfter(tt.in, now)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ParseRetryAfter(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if err := resilience.Sleep(t.Context(), 3*time.Second); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Errorf("slept %v, want 3s", elapsed)
		}

		cause := errors.New("shutdown")
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(time.Second, func() { cancel(cause) })
		start = time.Now()
		if err := resilience.Sleep(ctx, time.Hour); !errors.Is(err, cause) {
			t.Errorf("Sleep() error = %v, want cause", err)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("returned after %v, want 1s", elapsed)
		}
		if err := resilience.Sleep(ctx, 0); !errors.Is(err, cause) {
			t.Errorf("Sleep(done ctx, 0) = %v, want cause", err)
		}
		if err := resilience.Sleep(t.Context(), -time.Second); err != nil {
			t.Errorf("Sleep(-1s) = %v", err)
		}
	})
}
