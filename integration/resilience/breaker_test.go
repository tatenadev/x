// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package resilience_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration/resilience"
)

const cooldown = 30 * time.Second

func fail(t *testing.T, b *resilience.ConsecutiveBreaker, n int) {
	t.Helper()
	for range n {
		if err := b.Allow(); err != nil {
			t.Fatalf("Allow() = %v while counting failures", err)
		}
		b.Record(false)
	}
}

func wantState(t *testing.T, b *resilience.ConsecutiveBreaker, want resilience.State) {
	t.Helper()
	if got := b.State(); got != want {
		t.Fatalf("state = %v, want %v", got, want)
	}
}

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	b := resilience.NewConsecutiveBreaker(3, cooldown)
	fail(t, b, 2)
	b.Allow()
	b.Record(true) // resets the run
	fail(t, b, 2)
	wantState(t, b, resilience.Closed)
	fail(t, b, 1)
	wantState(t, b, resilience.Open)
	if err := b.Allow(); !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Errorf("Allow() = %v, want ErrCircuitOpen", err)
	}
}

func TestBreaker_Cooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := resilience.NewConsecutiveBreaker(1, cooldown)
		fail(t, b, 1)

		synctest.Sleep(cooldown - time.Nanosecond)
		if err := b.Allow(); !errors.Is(err, resilience.ErrCircuitOpen) {
			t.Fatalf("Allow() before cooldown = %v, want ErrCircuitOpen", err)
		}
		synctest.Sleep(time.Nanosecond)
		if err := b.Allow(); err != nil {
			t.Fatalf("probe Allow() = %v, want nil", err)
		}
		wantState(t, b, resilience.HalfOpen)
		if err := b.Allow(); !errors.Is(err, resilience.ErrCircuitOpen) {
			t.Fatalf("second Allow() in half-open = %v, want ErrCircuitOpen", err)
		}
	})
}

func TestBreaker_Probe(t *testing.T) {
	tests := []struct {
		name    string
		success bool
		want    resilience.State
	}{
		{name: "success closes", success: true, want: resilience.Closed},
		{name: "failure reopens", success: false, want: resilience.Open},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := resilience.NewConsecutiveBreaker(2, cooldown)
				fail(t, b, 2)
				synctest.Sleep(cooldown)
				if err := b.Allow(); err != nil {
					t.Fatal(err)
				}
				b.Record(tt.success)
				wantState(t, b, tt.want)

				if tt.success {
					// Closed again, with a fresh failure count.
					fail(t, b, 1)
					wantState(t, b, resilience.Closed)
					return
				}
				// Reopened for a full new cooldown.
				synctest.Sleep(cooldown - time.Nanosecond)
				if err := b.Allow(); !errors.Is(err, resilience.ErrCircuitOpen) {
					t.Errorf("Allow() = %v during the new cooldown", err)
				}
				synctest.Sleep(time.Nanosecond)
				if err := b.Allow(); err != nil {
					t.Errorf("Allow() after the new cooldown = %v", err)
				}
			})
		})
	}
}

func TestBreaker_AbandonedProbeIsReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := resilience.NewConsecutiveBreaker(1, cooldown)
		fail(t, b, 1)
		synctest.Sleep(cooldown)
		if err := b.Allow(); err != nil { // a probe that never records
			t.Fatal(err)
		}
		synctest.Sleep(cooldown - time.Nanosecond)
		if err := b.Allow(); !errors.Is(err, resilience.ErrCircuitOpen) {
			t.Fatalf("Allow() = %v while the probe may still report", err)
		}
		synctest.Sleep(time.Nanosecond)
		if err := b.Allow(); err != nil {
			t.Fatalf("Allow() = %v, want a replacement probe", err)
		}
	})
}

func TestBreaker_LateResultWhileOpenIgnored(t *testing.T) {
	b := resilience.NewConsecutiveBreaker(1, cooldown)
	b.Allow() // let through while closed...
	fail(t, b, 1)
	b.Record(true) // ...and reporting after the breaker opened
	wantState(t, b, resilience.Open)
}

func TestBreaker_OneProbeUnderContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := resilience.NewConsecutiveBreaker(1, cooldown)
		fail(t, b, 1)
		synctest.Sleep(cooldown)

		var allowed atomic.Int32
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				if b.Allow() == nil {
					allowed.Add(1)
				}
			})
		}
		wg.Wait()
		if n := allowed.Load(); n != 1 {
			t.Errorf("%d probes allowed, want 1", n)
		}
	})
}

func TestBreaker_ConcurrentRace(t *testing.T) {
	b := resilience.NewConsecutiveBreaker(3, time.Nanosecond)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			for j := range 50 {
				if b.Allow() == nil {
					b.Record((i+j)%3 != 0)
				}
				_ = b.State()
			}
		})
	}
	wg.Wait()
}

func TestStateString(t *testing.T) {
	for s, want := range map[resilience.State]string{
		resilience.Closed: "closed", resilience.Open: "open", resilience.HalfOpen: "half-open", 9: "unknown",
	} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}
