// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
)

func TestNewHTTPClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &integrationtest.RecordingTransport{Respond: integrationtest.Script(
			integrationtest.Status(503, ""), integrationtest.Status(404, ""))}
		hc, err := integration.NewHTTPClient("sdk",
			integration.WithBaseTransport(rt),
			integration.WithTimeout(time.Minute),
			integration.WithRetry(3, time.Millisecond, time.Millisecond),
			integration.WithTokenSource(auth.StaticToken("t")))
		if err != nil {
			t.Fatal(err)
		}
		if hc.Timeout != time.Minute {
			t.Errorf("Timeout = %v", hc.Timeout)
		}

		// Any host, and non-2xx responses come back as responses: the SDK
		// does its own error mapping.
		for _, host := range []string{"https://a.test/x", "https://b.test/y"} {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, host, nil)
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatalf("Do(%s) error = %v", host, err)
			}
			resp.Body.Close()
		}
		reqs := rt.Requests()
		if len(reqs) != 3 { // a.test: 503 then 404; b.test: 404
			t.Fatalf("attempts = %d, want 3", len(reqs))
		}
		for _, r := range reqs {
			if r.Header.Get("Authorization") != "Bearer t" || r.Header.Get("X-Request-Id") == "" {
				t.Errorf("%s: headers = %v", r.URL, r.Header)
			}
		}
	})
}

func TestNewHTTPClient_Invalid(t *testing.T) {
	tests := map[string][]integration.Option{
		"zero timeout":  {integration.WithTimeout(0)},
		"zero attempts": {integration.WithRetry(0, time.Millisecond, time.Millisecond)},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := integration.NewHTTPClient("sdk", opts...); !errors.Is(err, integration.ErrInvalidConfig) {
				t.Errorf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
	if _, err := integration.NewHTTPClient(""); !errors.Is(err, integration.ErrInvalidConfig) {
		t.Errorf("empty service: error = %v", err)
	}
}
