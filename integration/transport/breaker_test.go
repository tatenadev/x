// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/resilience"
	"github.com/tatenadev/x/integration/transport"
)

// recordingBreaker allows everything and remembers what was recorded.
type recordingBreaker struct {
	allowErr error
	records  []bool
}

func (b *recordingBreaker) Allow() error        { return b.allowErr }
func (b *recordingBreaker) Record(success bool) { b.records = append(b.records, success) }

func TestCircuitBreaker_Classification(t *testing.T) {
	tests := []struct {
		name    string
		respond integrationtest.RespondFunc
		ctx     func(context.Context) (context.Context, context.CancelFunc)
		want    []bool // nil: nothing recorded
	}{
		{name: "200", respond: integrationtest.Status(200, ""), want: []bool{true}},
		{name: "400", respond: integrationtest.Status(400, ""), want: []bool{true}},
		{name: "401", respond: integrationtest.Status(401, ""), want: []bool{true}},
		{name: "404", respond: integrationtest.Status(404, ""), want: []bool{true}},
		{name: "429", respond: integrationtest.Status(429, ""), want: []bool{true}},
		{name: "500", respond: integrationtest.Status(500, ""), want: []bool{false}},
		{name: "501", respond: integrationtest.Status(501, ""), want: []bool{false}},
		{name: "503", respond: integrationtest.Status(503, ""), want: []bool{false}},
		{name: "network error", respond: integrationtest.Fail(errors.New("reset")), want: []bool{false}},
		{name: "token source down", respond: integrationtest.Fail(fmt.Errorf("%w: idp down", auth.ErrTokenUnavailable))},
		{
			name: "caller canceled", respond: integrationtest.Fail(context.Canceled),
			ctx: func(ctx context.Context) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(ctx)
				cancel()
				return ctx, cancel
			},
		},
		{
			name: "attempt timeout", respond: integrationtest.Fail(context.DeadlineExceeded), want: []bool{false},
			ctx: func(ctx context.Context) (context.Context, context.CancelFunc) {
				return context.WithTimeoutCause(ctx, -time.Second, resilience.ErrAttemptTimeout)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.ctx != nil {
				var cancel context.CancelFunc
				ctx, cancel = tt.ctx(ctx)
				defer cancel()
			}
			b := &recordingBreaker{}
			base := &integrationtest.RecordingTransport{Respond: tt.respond}
			resp, err := transport.Chain(base, transport.CircuitBreaker(b)).RoundTrip(newReq(t, ctx, http.MethodGet, nil))
			if err == nil {
				resp.Body.Close()
			}
			if fmt.Sprint(b.records) != fmt.Sprint(tt.want) {
				t.Errorf("recorded %v, want %v", b.records, tt.want)
			}
		})
	}
}

func TestCircuitBreaker_RejectsWhenOpen(t *testing.T) {
	b := &recordingBreaker{allowErr: resilience.ErrCircuitOpen}
	base := &integrationtest.RecordingTransport{}
	body := integrationtest.NewBody("payload")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.test/x", body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.Chain(base, transport.CircuitBreaker(b)).RoundTrip(req)
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("error = %v, want ErrCircuitOpen", err)
	}
	if !body.Closed() {
		t.Error("request body not closed")
	}
	if len(base.Requests()) != 0 || b.records != nil {
		t.Error("a rejected request was sent or recorded")
	}
}

func TestCircuitBreaker_PassesRequestUnchanged(t *testing.T) {
	base := &integrationtest.RecordingTransport{}
	req := newReq(t, t.Context(), http.MethodGet, nil)
	resp, err := transport.Chain(base, transport.CircuitBreaker(&recordingBreaker{})).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if base.Requests()[0] != req || len(req.Header) != 0 {
		t.Error("the breaker must forward the request untouched")
	}
}

func TestRetry_StopsOnOpenCircuit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := &integrationtest.RecordingTransport{Respond: integrationtest.Status(503, "")}
		rt := transport.Chain(base,
			transport.Retry(defaultRetry(10)),
			transport.CircuitBreaker(resilience.NewConsecutiveBreaker(2, time.Minute)))
		_, err := rt.RoundTrip(newReq(t, t.Context(), http.MethodGet, nil))
		if !errors.Is(err, resilience.ErrCircuitOpen) {
			t.Fatalf("error = %v, want ErrCircuitOpen", err)
		}
		if n := len(base.Requests()); n != 2 {
			t.Errorf("upstream saw %d attempts, want 2 (then the breaker opened)", n)
		}
	})
}

func TestRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := &integrationtest.RecordingTransport{}
		rt := transport.Chain(base, transport.RateLimit(resilience.NewTokenBucket(2, 1)))
		start := time.Now()
		for range 3 {
			req := newReq(t, t.Context(), http.MethodGet, nil)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if base.Requests()[len(base.Requests())-1] != req {
				t.Error("rate limiting must forward the request untouched")
			}
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("3 calls at 2/s with burst 1 took %v, want 1s", elapsed)
		}
	})
}

func TestRateLimit_CanceledWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := &integrationtest.RecordingTransport{}
		l := resilience.NewTokenBucket(1, 1)
		l.Wait(t.Context())

		cause := errors.New("caller gave up")
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(100*time.Millisecond, func() { cancel(cause) })
		body := integrationtest.NewBody("payload")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.test/x", body)
		if err != nil {
			t.Fatal(err)
		}
		_, err = transport.Chain(base, transport.RateLimit(l)).RoundTrip(req)
		if !errors.Is(err, cause) {
			t.Fatalf("error = %v, want the cancellation cause", err)
		}
		if !body.Closed() || len(base.Requests()) != 0 {
			t.Error("canceled request was sent or its body left open")
		}
	})
}
