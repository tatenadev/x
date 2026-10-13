// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/integrationtest"
)

func TestClient_BreakerOpensAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &integrationtest.RecordingTransport{Respond: integrationtest.Script(
			integrationtest.Status(503, ""), integrationtest.Status(503, ""), integrationtest.Status(204, ""))}
		c := newClient(t, "https://api.test", rt, integration.WithBreaker(2, 10*time.Second))

		for range 2 {
			if e, ok := errors.AsType[*integration.Error](c.Delete(t.Context(), "/x")); !ok || e.StatusCode != 503 {
				t.Fatalf("want a 503 *Error, got %v", e)
			}
		}
		err := c.Delete(t.Context(), "/x")
		if !errors.Is(err, integration.ErrCircuitOpen) {
			t.Fatalf("third call: %v, want ErrCircuitOpen", err)
		}
		if integration.IsRetryable(err) {
			t.Error("ErrCircuitOpen reported as retryable")
		}
		if n := len(rt.Requests()); n != 2 {
			t.Errorf("upstream saw %d requests, want 2", n)
		}

		time.Sleep(10 * time.Second)
		if err := c.Delete(t.Context(), "/x"); err != nil {
			t.Fatalf("probe after cooldown: %v", err)
		}
		if err := c.Delete(t.Context(), "/x"); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})
}

func TestClient_RetryStopsOnOpenCircuit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &integrationtest.RecordingTransport{Respond: integrationtest.Status(503, "")}
		c := newClient(t, "https://api.test", rt,
			integration.WithRetry(10, 10*time.Millisecond, 100*time.Millisecond),
			integration.WithBreaker(3, time.Minute))
		err := c.Delete(t.Context(), "/x")
		if !errors.Is(err, integration.ErrCircuitOpen) {
			t.Fatalf("error = %v, want ErrCircuitOpen", err)
		}
		if n := len(rt.Requests()); n != 3 {
			t.Errorf("upstream saw %d attempts, want 3", n)
		}
	})
}

func TestClient_FourXXDoesNotOpenBreaker(t *testing.T) {
	rt := &integrationtest.RecordingTransport{Respond: integrationtest.Status(404, "")}
	c := newClient(t, "https://api.test", rt, integration.WithBreaker(2, time.Minute))
	for range 10 {
		if err := c.Delete(t.Context(), "/x"); errors.Is(err, integration.ErrCircuitOpen) {
			t.Fatal("breaker opened on 4xx responses")
		}
	}
	if n := len(rt.Requests()); n != 10 {
		t.Errorf("upstream saw %d requests, want 10", n)
	}
}

func TestClient_RateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &integrationtest.RecordingTransport{}
		c := newClient(t, "https://api.test", rt, integration.WithRateLimit(4, 2))
		start := time.Now()
		for range 6 {
			if err := c.Delete(t.Context(), "/x"); err != nil {
				t.Fatal(err)
			}
		}
		// 2 from the burst, then 4 more at 250ms each.
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("6 calls took %v, want 1s", elapsed)
		}
	})
}

func TestNew_InvalidResilienceOptions(t *testing.T) {
	tests := map[string]integration.Option{
		"zero failures":               integration.WithBreaker(0, time.Second),
		"zero cooldown":               integration.WithBreaker(1, 0),
		"negative rate":               integration.WithRateLimit(-1, 1),
		"zero burst":                  integration.WithRateLimit(1, 0),
		"slower than attempt timeout": integration.WithRateLimit(0.05, 1), // 20s apart vs 10s attempts
	}
	for name, opt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := integration.New("s", "https://api.test", opt); !errors.Is(err, integration.ErrInvalidConfig) {
				t.Errorf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
