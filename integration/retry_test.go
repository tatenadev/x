// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/resilience"
	"github.com/tatenadev/x/integration/transport"
)

func seededRand() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

// statusSequence serves the given statuses in order (the last repeats) and
// records each request's headers.
type statusSequence struct {
	mu       sync.Mutex
	statuses []int
	headers  []http.Header
}

func (s *statusSequence) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	i := min(len(s.headers), len(s.statuses)-1)
	s.headers = append(s.headers, r.Header.Clone())
	s.mu.Unlock()
	w.WriteHeader(s.statuses[i])
}

func (s *statusSequence) seen() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers
}

func TestRetry_EndToEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seq := &statusSequence{statuses: []int{503, 503, 204}}
		srv := integrationtest.Server(t, seq)
		c, err := integration.New("test", srv.URL,
			integration.WithBaseTransport(srv.Client().Transport),
			integration.WithRand(seededRand()),
		)
		if err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		if err := c.Delete(t.Context(), "/x"); err != nil {
			t.Fatal(err)
		}
		ref := resilience.NewBackoff(integration.DefaultBackoffBase, integration.DefaultBackoffMax, seededRand())
		if elapsed, want := time.Since(start), ref.Delay(1)+ref.Delay(2); elapsed != want {
			t.Errorf("elapsed %v, want exactly the two backoff delays %v", elapsed, want)
		}
		if n := len(seq.seen()); n != 3 {
			t.Errorf("server saw %d requests, want 3", n)
		}
	})
}

func TestRetry_PostIdempotency(t *testing.T) {
	tests := []struct {
		name     string
		opts     []integration.Option
		ctx      func(context.Context) context.Context
		attempts int
	}{
		{name: "not opted in: no key, no retry", attempts: 1},
		{name: "per call", ctx: integration.Idempotent, attempts: 3},
		{name: "client-wide", opts: []integration.Option{integration.WithIdempotencyKeys()}, attempts: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				seq := &statusSequence{statuses: []int{503, 502, 201}}
				srv := integrationtest.Server(t, seq)
				opts := append([]integration.Option{
					integration.WithBaseTransport(srv.Client().Transport),
					integration.WithRand(seededRand()),
				}, tt.opts...)
				c, err := integration.New("billing", srv.URL, opts...)
				if err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				if tt.ctx != nil {
					ctx = tt.ctx(ctx)
				}
				c.PostJSON[invoice, struct{}](ctx, "/invoices", invoice{ID: "a"})

				seen := seq.seen()
				if len(seen) != tt.attempts {
					t.Fatalf("server saw %d attempts, want %d", len(seen), tt.attempts)
				}
				key := seen[0].Get("Idempotency-Key")
				if (key != "") != (tt.attempts > 1) {
					t.Errorf("Idempotency-Key = %q", key)
				}
				for i, h := range seen {
					if h.Get("Idempotency-Key") != key || h.Get("X-Request-Id") != seen[0].Get("X-Request-Id") {
						t.Errorf("attempt %d carried different keys or IDs", i+1)
					}
				}
			})
		})
	}
}

// rotatingTokens issues "tok-N" and moves on when invalidated.
type rotatingTokens struct {
	gen, invalidated atomic.Int32
}

func (r *rotatingTokens) Token(context.Context) (auth.Token, error) {
	return auth.Token{Type: "Bearer", Value: "tok-" + strconv.Itoa(int(r.gen.Load())+1)}, nil
}

func (r *rotatingTokens) Invalidate() {
	r.gen.Add(1)
	r.invalidated.Add(1)
}

func TestRetry_Unauthorized_RefreshesToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var seen []string
		var mu sync.Mutex
		srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer tok-2" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		src := &rotatingTokens{}
		c := newClient(t, srv.URL, srv.Client().Transport, integration.WithRetry(3, time.Second, time.Second),
			integration.WithTokenSource(src))

		start := time.Now()
		if err := c.Delete(t.Context(), "/x"); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		if time.Since(start) != 0 {
			t.Errorf("waited %v before retrying the 401", time.Since(start))
		}
		if len(seen) != 2 || seen[0] != "Bearer tok-1" || seen[1] != "Bearer tok-2" {
			t.Errorf("tokens sent = %v", seen)
		}
		if n := src.invalidated.Load(); n != 1 {
			t.Errorf("Invalidate called %d times, want 1", n)
		}
	})
}

func TestError_RequestID(t *testing.T) {
	tests := []struct {
		name    string
		respond integrationtest.RespondFunc
		ctxID   string
	}{
		{name: "status error", respond: integrationtest.Status(http.StatusBadRequest, "")},
		{name: "transport error", respond: integrationtest.Fail(errors.New("reset"))},
		{name: "propagated from context", respond: integrationtest.Fail(errors.New("reset")), ctxID: "inbound-7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{Respond: tt.respond}
			ctx := t.Context()
			if tt.ctxID != "" {
				ctx = transport.WithRequestID(ctx, tt.ctxID)
			}
			_, err := newClient(t, "https://api.test", rt).Do(ctx, newRequest(t, http.MethodGet, "https://api.test/x"))
			e, ok := errors.AsType[*integration.Error](err)
			if !ok {
				t.Fatalf("error = %v", err)
			}
			sent := rt.Requests()[0].Header.Get("X-Request-ID")
			if e.RequestID == "" || e.RequestID != sent {
				t.Errorf("Error.RequestID = %q, sent X-Request-ID = %q", e.RequestID, sent)
			}
			if tt.ctxID != "" && sent != tt.ctxID {
				t.Errorf("sent X-Request-ID = %q, want %q", sent, tt.ctxID)
			}
		})
	}
}

func TestRetry_ServerErrors(t *testing.T) {
	tests := []struct {
		name      string
		opts      []integration.Option
		attempts  int
		retryable bool
	}{
		{name: "500 not retried by default", attempts: 1},
		{name: "500 retried when configured", opts: []integration.Option{integration.WithRetryServerErrors()}, attempts: 3, retryable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt := &integrationtest.RecordingTransport{Respond: integrationtest.Status(500, "")}
				opts := append([]integration.Option{integration.WithRetry(3, time.Millisecond, time.Second)}, tt.opts...)
				err := newClient(t, "https://api.test", rt, opts...).Delete(t.Context(), "/x")
				if n := len(rt.Requests()); n != tt.attempts {
					t.Errorf("attempts = %d, want %d", n, tt.attempts)
				}
				if integration.IsRetryable(err) != tt.retryable {
					t.Errorf("IsRetryable(%v) = %v, want %v", err, !tt.retryable, tt.retryable)
				}
			})
		})
	}
}

func TestRetry_AttemptTimeoutThroughClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		c := newClient(t, srv.URL, srv.Client().Transport,
			integration.WithRetry(2, time.Millisecond, time.Millisecond),
			integration.WithAttemptTimeout(time.Second))
		if err := c.Delete(t.Context(), "/x"); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2", calls.Load())
		}
	})
}

type neverRetry struct{}

func (neverRetry) ShouldRetry(*http.Request, *http.Response, error, int) (bool, time.Duration) {
	return false, 0
}

func TestWithRetryPolicy(t *testing.T) {
	rt := &integrationtest.RecordingTransport{Respond: integrationtest.Status(503, "")}
	c := newClient(t, "https://api.test", rt, integration.WithRetry(5, time.Millisecond, time.Second),
		integration.WithRetryPolicy(neverRetry{}))
	c.Delete(t.Context(), "/x")
	if n := len(rt.Requests()); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestNew_InvalidRetryOptions(t *testing.T) {
	for _, opt := range []integration.Option{integration.WithRand(nil), integration.WithRetryPolicy(nil)} {
		if _, err := integration.New("s", "https://api.test", opt); !errors.Is(err, integration.ErrInvalidConfig) {
			t.Errorf("New() error = %v, want ErrInvalidConfig", err)
		}
	}
}
