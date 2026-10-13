// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package resilience

import (
	"sync"
	"time"
)

// Breaker guards an upstream: Allow before each call, Record its outcome.
type Breaker interface {
	// Allow returns nil if a call may proceed, or ErrCircuitOpen.
	Allow() error
	// Record reports the outcome of a call that Allow let through.
	Record(success bool)
}

// State is a circuit breaker state.
type State int

// Breaker states.
const (
	Closed   State = iota // calls flow; failures are counted
	Open                  // calls are rejected until the cooldown ends
	HalfOpen              // one probe call decides between Closed and Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	}
	return "unknown"
}

// ConsecutiveBreaker opens after a run of consecutive failures, rejects
// calls for a cooldown, then lets one probe through: success closes it,
// failure opens it for another cooldown. A probe that never reports back
// (its caller gave up) is replaced by a new one after a further cooldown.
// It is safe for concurrent use.
type ConsecutiveBreaker struct {
	threshold int
	cooldown  time.Duration
	onChange  func(from, to State)

	mu           sync.Mutex
	state        State
	failures     int       // consecutive failures while closed
	openedAt     time.Time // when the breaker last opened
	probing      bool      // a half-open probe is in flight
	probeStarted time.Time
}

// BreakerOption configures [NewConsecutiveBreaker].
type BreakerOption func(*ConsecutiveBreaker)

// OnStateChange calls fn after every state transition, outside the
// breaker's lock. fn must not block.
func OnStateChange(fn func(from, to State)) BreakerOption {
	return func(b *ConsecutiveBreaker) { b.onChange = fn }
}

// NewConsecutiveBreaker returns a closed breaker that opens after
// threshold consecutive failures (minimum 1) and stays open for cooldown.
func NewConsecutiveBreaker(threshold int, cooldown time.Duration, opts ...BreakerOption) *ConsecutiveBreaker {
	b := &ConsecutiveBreaker{threshold: max(threshold, 1), cooldown: cooldown}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// transition runs fn under the lock and reports any state change to the
// OnStateChange callback after releasing it.
func (b *ConsecutiveBreaker) transition(fn func() error) error {
	b.mu.Lock()
	from := b.state
	err := fn()
	to := b.state
	b.mu.Unlock()
	if from != to && b.onChange != nil {
		b.onChange(from, to)
	}
	return err
}

// Allow implements [Breaker].
func (b *ConsecutiveBreaker) Allow() error {
	return b.transition(b.allow)
}

// allow is Allow with b.mu held.
func (b *ConsecutiveBreaker) allow() error {
	now := time.Now()
	switch b.state {
	case Open:
		if now.Sub(b.openedAt) < b.cooldown {
			return ErrCircuitOpen
		}
		b.state = HalfOpen
	case HalfOpen:
		if b.probing && now.Sub(b.probeStarted) < b.cooldown {
			return ErrCircuitOpen
		}
	default:
		return nil
	}
	b.probing, b.probeStarted = true, now
	return nil
}

// Record implements [Breaker]. A result arriving while open belongs to a
// call let through before the breaker opened and is ignored.
func (b *ConsecutiveBreaker) Record(success bool) {
	b.transition(func() error {
		b.record(success)
		return nil
	})
}

// record is Record with b.mu held.
func (b *ConsecutiveBreaker) record(success bool) {
	switch b.state {
	case Closed:
		if success {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.threshold {
			b.open()
		}
	case HalfOpen:
		if success {
			b.state, b.failures, b.probing = Closed, 0, false
			return
		}
		b.open()
	}
}

// open moves to Open. b.mu must be held.
func (b *ConsecutiveBreaker) open() {
	b.state, b.openedAt, b.probing, b.failures = Open, time.Now(), false, 0
}

// State returns the current state. An Open breaker whose cooldown has
// passed reports Open until the next Allow moves it to HalfOpen.
func (b *ConsecutiveBreaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
