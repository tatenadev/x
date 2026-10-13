// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/resilience"
	"github.com/tatenadev/x/integration/transport"
)

const (
	testBase = 100 * time.Millisecond
	testMax  = 5 * time.Second
)

func seeded() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

func defaultRetry(maxAttempts int) transport.RetryConfig {
	return transport.RetryConfig{
		Policy:      transport.DefaultPolicy{Backoff: resilience.NewBackoff(testBase, testMax, seeded())},
		MaxAttempts: maxAttempts,
	}
}

func newReq(t *testing.T, ctx context.Context, method string, body []byte) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body) // sets GetBody
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.test/x", r)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRetry_MethodsAndStatuses(t *testing.T) {
	netErr := errors.New("connection reset")
	outcomes := []struct {
		name      string
		respond   integrationtest.RespondFunc
		retryable bool
		with500   bool // run with RetryServerError, which makes it retryable
	}{
		{name: "429", respond: integrationtest.Status(429, ""), retryable: true},
		{name: "502", respond: integrationtest.Status(502, ""), retryable: true},
		{name: "503", respond: integrationtest.Status(503, ""), retryable: true},
		{name: "504", respond: integrationtest.Status(504, ""), retryable: true},
		{name: "network error", respond: integrationtest.Fail(netErr), retryable: true},
		{name: "500", respond: integrationtest.Status(500, "")},
		{name: "500 configured", respond: integrationtest.Status(500, ""), with500: true},
		{name: "501", respond: integrationtest.Status(501, "")},
		{name: "400", respond: integrationtest.Status(400, "")},
		{name: "401 without auth", respond: integrationtest.Status(401, "")},
		{name: "403", respond: integrationtest.Status(403, "")},
		{name: "404", respond: integrationtest.Status(404, "")},
		{name: "409", respond: integrationtest.Status(409, "")},
		{name: "circuit open", respond: integrationtest.Fail(resilience.ErrCircuitOpen)},
		{name: "rate limited", respond: integrationtest.Fail(resilience.ErrRateLimited)},
		{name: "no token", respond: integrationtest.Fail(auth.ErrNoToken)},
		{name: "200", respond: integrationtest.Status(200, "")},
	}
	methods := []struct {
		name       string
		method     string
		key        bool
		idempotent bool
	}{
		{name: "GET", method: http.MethodGet, idempotent: true},
		{name: "HEAD", method: http.MethodHead, idempotent: true},
		{name: "OPTIONS", method: http.MethodOptions, idempotent: true},
		{name: "PUT", method: http.MethodPut, idempotent: true},
		{name: "DELETE", method: http.MethodDelete, idempotent: true},
		{name: "POST", method: http.MethodPost},
		{name: "PATCH", method: http.MethodPatch},
		{name: "POST with key", method: http.MethodPost, key: true, idempotent: true},
		{name: "PATCH with key", method: http.MethodPatch, key: true, idempotent: true},
		{name: "TRACE", method: http.MethodTrace},
	}
	for _, m := range methods {
		for _, o := range outcomes {
			t.Run(m.name+"/"+o.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					base := &integrationtest.RecordingTransport{Respond: o.respond}
					cfg := defaultRetry(3)
					cfg.Policy = transport.DefaultPolicy{
						Backoff:          resilience.NewBackoff(testBase, testMax, seeded()),
						RetryServerError: o.with500,
					}
					req := newReq(t, t.Context(), m.method, nil)
					if m.key {
						req.Header.Set(transport.HeaderIdempotencyKey, "k")
					}
					resp, err := transport.Chain(base, transport.Retry(cfg)).RoundTrip(req)
					if err == nil {
						resp.Body.Close()
					}
					want := 1
					if m.idempotent && (o.retryable || o.with500) {
						want = 3
					}
					if got := len(base.Requests()); got != want {
						t.Errorf("attempts = %d, want %d", got, want)
					}
				})
			})
		}
	}
}

func TestRetry_BodyReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var bodies []string
		var mu sync.Mutex
		base := &integrationtest.RecordingTransport{Respond: func(req *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(req.Body)
			req.Body.Close()
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
			return integrationtest.Response(http.StatusServiceUnavailable, ""), nil
		}}
		req := newReq(t, t.Context(), http.MethodPut, []byte("payload"))
		resp, err := transport.Chain(base, transport.Retry(defaultRetry(3))).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(bodies) != 3 || bodies[0] != "payload" || bodies[1] != "payload" || bodies[2] != "payload" {
			t.Errorf("bodies = %q, want payload x3", bodies)
		}
	})
}

func TestRetry_NoGetBodyMeansOneAttempt(t *testing.T) {
	base := &integrationtest.RecordingTransport{Respond: integrationtest.Status(http.StatusServiceUnavailable, "")}
	body := integrationtest.NewBody("payload") // a plain ReadCloser: no GetBody
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://api.test/x", body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.Chain(base, transport.Retry(defaultRetry(3))).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := len(base.Requests()); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestRetry_ClosesDiscardedResponses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var bodies []*integrationtest.Body
		base := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
			resp := integrationtest.Response(http.StatusBadGateway, "bad gateway")
			mu.Lock()
			bodies = append(bodies, resp.Body.(*integrationtest.Body))
			mu.Unlock()
			return resp, nil
		}}
		resp, err := transport.Chain(base, transport.Retry(defaultRetry(3))).RoundTrip(newReq(t, t.Context(), http.MethodGet, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !bodies[0].Closed() || !bodies[1].Closed() {
			t.Error("discarded responses were not closed")
		}
		if bodies[2].Closed() {
			t.Error("returned response closed before the caller closed it")
		}
		resp.Body.Close()
		if !bodies[2].Closed() {
			t.Error("closing the returned body did not close the underlying body")
		}
	})
}

func TestRetry_DoesNotMutateRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := &integrationtest.RecordingTransport{Respond: integrationtest.Status(http.StatusServiceUnavailable, "")}
		req := newReq(t, t.Context(), http.MethodPut, []byte("payload"))
		origBody, origCtx := req.Body, req.Context()
		resp, err := transport.Chain(base, transport.Retry(defaultRetry(3))).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if req.Body != origBody || req.Context() != origCtx || len(req.Header) != 0 {
			t.Error("Retry modified the caller's request")
		}
		for i, sent := range base.Requests() {
			if sent == req {
				t.Errorf("attempt %d sent the caller's request instead of a clone", i+1)
			}
		}
	})
}

// countingServer answers each request with the next handler in steps; the
// last one repeats.
func countingServer(t *testing.T, steps ...http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := min(int(n.Add(1)), len(steps)) - 1
		steps[i](w, r)
	}))
	return srv, &n
}

func status(code int, headers ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
	}
}

func hang(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func roundTripServer(t *testing.T, srv *httptest.Server, cfg transport.RetryConfig, ctx context.Context) (*http.Response, error, time.Duration) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := transport.Chain(srv.Client().Transport, transport.Retry(cfg)).RoundTrip(req)
	return resp, err, time.Since(start)
}

func TestRetry_BackoffTiming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, calls := countingServer(t, status(503), status(503), status(200))
		resp, err, elapsed := roundTripServer(t, srv, defaultRetry(3), t.Context())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || calls.Load() != 3 {
			t.Fatalf("status %d after %d calls", resp.StatusCode, calls.Load())
		}
		// Same seed, same jitter: the wait is exactly the two delays.
		ref := resilience.NewBackoff(testBase, testMax, seeded())
		if want := ref.Delay(1) + ref.Delay(2); elapsed != want {
			t.Errorf("elapsed %v, want %v", elapsed, want)
		}
	})
}

func TestRetry_RetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter func(now time.Time) string
		want       time.Duration
	}{
		{name: "seconds", retryAfter: func(time.Time) string { return "2" }, want: 2 * time.Second},
		{name: "http-date", retryAfter: func(now time.Time) string {
			return now.Add(3 * time.Second).UTC().Format(http.TimeFormat)
		}, want: 3 * time.Second},
		{name: "capped at backoff max", retryAfter: func(time.Time) string { return "100" }, want: testMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				srv, calls := countingServer(t,
					func(w http.ResponseWriter, r *http.Request) {
						status(http.StatusTooManyRequests, "Retry-After", tt.retryAfter(time.Now()))(w, r)
					},
					status(200))
				resp, err, elapsed := roundTripServer(t, srv, defaultRetry(3), t.Context())
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if calls.Load() != 2 || elapsed != tt.want {
					t.Errorf("%d calls after %v, want 2 after %v", calls.Load(), elapsed, tt.want)
				}
			})
		})
	}
}

func TestRetry_GivesUpWhenWaitOutlastsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, calls := countingServer(t, status(http.StatusServiceUnavailable, "Retry-After", "4"))
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		resp, err, elapsed := roundTripServer(t, srv, defaultRetry(3), ctx)
		if err != nil {
			t.Fatalf("error = %v; want the last 503", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 503 || calls.Load() != 1 || elapsed != 0 {
			t.Errorf("status %d, %d calls, %v elapsed; want 503 at once", resp.StatusCode, calls.Load(), elapsed)
		}
	})
}

func TestRetry_AttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, calls := countingServer(t, hang, status(200))
		cfg := defaultRetry(3)
		cfg.AttemptTimeout = time.Second
		resp, err, elapsed := roundTripServer(t, srv, cfg, t.Context())
		if err != nil {
			t.Fatalf("error = %v; a timed-out attempt must be retried", err)
		}
		resp.Body.Close()
		ref := resilience.NewBackoff(testBase, testMax, seeded())
		if want := time.Second + ref.Delay(1); calls.Load() != 2 || elapsed != want {
			t.Errorf("%d calls after %v, want 2 after %v", calls.Load(), elapsed, want)
		}
	})
}

func TestRetry_AttemptTimeoutExhausted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, calls := countingServer(t, hang)
		cfg := defaultRetry(2)
		cfg.AttemptTimeout = time.Second
		_, err, _ := roundTripServer(t, srv, cfg, t.Context())
		if !errors.Is(err, resilience.ErrAttemptTimeout) {
			t.Errorf("error = %v, want ErrAttemptTimeout", err)
		}
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2", calls.Load())
		}
	})
}

func TestRetry_CallerCancellation(t *testing.T) {
	tests := []struct {
		name     string
		steps    []http.HandlerFunc
		cancelAt time.Duration
	}{
		{name: "during attempt", steps: []http.HandlerFunc{hang}, cancelAt: time.Second},
		// The first backoff delay with the seeded source is well over 1ms.
		{name: "during backoff", steps: []http.HandlerFunc{status(503)}, cancelAt: time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				srv, calls := countingServer(t, tt.steps...)
				cause := errors.New("caller gave up")
				ctx, cancel := context.WithCancelCause(t.Context())
				time.AfterFunc(tt.cancelAt, func() { cancel(cause) })
				_, err, elapsed := roundTripServer(t, srv, defaultRetry(5), ctx)
				if !errors.Is(err, cause) {
					t.Errorf("error = %v, want it to wrap the cancellation cause", err)
				}
				if calls.Load() != 1 || elapsed != tt.cancelAt {
					t.Errorf("%d calls, stopped after %v; want 1 call, stop at %v", calls.Load(), elapsed, tt.cancelAt)
				}
			})
		})
	}
}

func TestRetry_IdempotencyKeyStable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := &integrationtest.RecordingTransport{Respond: integrationtest.Script(
			integrationtest.Status(503, ""), integrationtest.Status(502, ""), integrationtest.Status(201, ""))}
		rt := transport.Chain(base, transport.RequestID(transport.IdempotencyKeys()), transport.Retry(defaultRetry(3)))
		resp, err := rt.RoundTrip(newReq(t, t.Context(), http.MethodPost, []byte(`{"amount":5}`)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		reqs := base.Requests()
		if len(reqs) != 3 {
			t.Fatalf("attempts = %d, want 3", len(reqs))
		}
		key, id := reqs[0].Header.Get(transport.HeaderIdempotencyKey), reqs[0].Header.Get(transport.HeaderRequestID)
		if key == "" || id == "" {
			t.Fatalf("first attempt key=%q id=%q", key, id)
		}
		for i, r := range reqs {
			if r.Header.Get(transport.HeaderIdempotencyKey) != key || r.Header.Get(transport.HeaderRequestID) != id {
				t.Errorf("attempt %d: key=%q id=%q; want %q %q", i+1,
					r.Header.Get(transport.HeaderIdempotencyKey), r.Header.Get(transport.HeaderRequestID), key, id)
			}
		}
	})
}

func TestRetry_Unauthorized(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		enabled  bool
		steps    []integrationtest.RespondFunc
		want     int
		attempts int
	}{
		{name: "retried once, then succeeds", enabled: true,
			steps: []integrationtest.RespondFunc{integrationtest.Status(401, ""), integrationtest.Status(200, "")}, want: 200, attempts: 2},
		{name: "retried only once", enabled: true,
			steps: []integrationtest.RespondFunc{integrationtest.Status(401, "")}, want: 401, attempts: 2},
		{name: "POST retried too: a 401 was never processed", method: http.MethodPost, enabled: true,
			steps: []integrationtest.RespondFunc{integrationtest.Status(401, ""), integrationtest.Status(200, "")}, want: 200, attempts: 2},
		{name: "disabled", steps: []integrationtest.RespondFunc{integrationtest.Status(401, "")}, want: 401, attempts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base := &integrationtest.RecordingTransport{Respond: integrationtest.Script(tt.steps...)}
				cfg := defaultRetry(3)
				cfg.RetryUnauthorized = tt.enabled
				method := tt.method
				if method == "" {
					method = http.MethodGet
				}
				start := time.Now()
				resp, err := transport.Chain(base, transport.Retry(cfg)).RoundTrip(newReq(t, t.Context(), method, []byte("x")))
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != tt.want || len(base.Requests()) != tt.attempts {
					t.Errorf("status %d after %d attempts, want %d after %d", resp.StatusCode, len(base.Requests()), tt.want, tt.attempts)
				}
				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("waited %v before the 401 retry, want none", elapsed)
				}
			})
		})
	}
}

func TestRetry_ConcurrentRace(t *testing.T) {
	var n atomic.Int32
	base := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		if n.Add(1)%2 == 0 {
			return integrationtest.Response(503, ""), nil
		}
		return integrationtest.Response(200, ""), nil
	}}
	cfg := transport.RetryConfig{
		Policy:      transport.DefaultPolicy{Backoff: resilience.NewBackoff(0, 0, seeded())}, // no waiting
		MaxAttempts: 3,
	}
	rt := transport.Chain(base, transport.Retry(cfg))
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			req := newReq(t, t.Context(), http.MethodPut, []byte(strconv.Itoa(i)))
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		})
	}
	wg.Wait()
}
