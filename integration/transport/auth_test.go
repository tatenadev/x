// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/transport"
)

// rotatingSource issues "tok-N", moving to the next N on Invalidate.
type rotatingSource struct {
	mu          sync.Mutex
	gen         int
	invalidated int
	err         error
}

func (s *rotatingSource) Token(context.Context) (auth.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return auth.Token{}, s.err
	}
	return auth.Token{Type: "Bearer", Value: "tok-" + strconv.Itoa(s.gen+1)}, nil
}

func (s *rotatingSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	s.invalidated++
}

func (s *rotatingSource) invalidations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidated
}

func authRequest(t *testing.T, header http.Header) (*http.Request, *integrationtest.Body) {
	t.Helper()
	body := integrationtest.NewBody("payload")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.test/x", body)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	return req, body
}

func TestAuth_SetsHeader(t *testing.T) {
	tests := []struct {
		name       string
		src        auth.TokenSource
		opts       []transport.AuthOption
		existing   http.Header
		wantHeader string
		wantValue  string
	}{
		{name: "bearer", src: auth.StaticToken("t"), wantHeader: "Authorization", wantValue: "Bearer t"},
		{name: "api key", src: auth.APIKey("X-Api-Key", "k"), wantHeader: "X-Api-Key", wantValue: "k"},
		{name: "existing kept", src: auth.StaticToken("t"),
			existing: http.Header{"Authorization": {"Bearer caller"}}, wantHeader: "Authorization", wantValue: "Bearer caller"},
		{name: "existing overwritten when configured", src: auth.StaticToken("t"), opts: []transport.AuthOption{transport.OverwriteAuth()},
			existing: http.Header{"Authorization": {"Bearer caller"}}, wantHeader: "Authorization", wantValue: "Bearer t"},
		{name: "other header does not block", src: auth.APIKey("X-Api-Key", "k"),
			existing: http.Header{"Authorization": {"Bearer caller"}}, wantHeader: "X-Api-Key", wantValue: "k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &integrationtest.RecordingTransport{}
			rt := transport.Chain(base, transport.Auth(tt.src, tt.opts...))
			req, _ := authRequest(t, tt.existing)
			before := req.Header.Clone()

			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			sent := base.Requests()[0]
			if got := sent.Header.Get(tt.wantHeader); got != tt.wantValue {
				t.Errorf("%s = %q, want %q", tt.wantHeader, got, tt.wantValue)
			}
			if len(req.Header) != len(before) || req.Header.Get("Authorization") != before.Get("Authorization") ||
				req.Header.Get(tt.wantHeader) != before.Get(tt.wantHeader) {
				t.Errorf("caller's request mutated: %v, was %v", req.Header, before)
			}
		})
	}
}

func TestAuth_TokenErrorClosesBody(t *testing.T) {
	src := &rotatingSource{err: errors.New("idp down")}
	base := &integrationtest.RecordingTransport{}
	req, body := authRequest(t, nil)
	_, err := transport.Chain(base, transport.Auth(src)).RoundTrip(req)
	if err == nil || !errors.Is(err, src.err) {
		t.Fatalf("RoundTrip() error = %v, want it to wrap the source error", err)
	}
	if !body.Closed() {
		t.Error("request body not closed on the error path")
	}
	if n := len(base.Requests()); n != 0 {
		t.Errorf("sent %d requests, want 0", n)
	}
}

func TestAuth_401InvalidatesOnce(t *testing.T) {
	src := &rotatingSource{}
	var respBody *integrationtest.Body
	base := &integrationtest.RecordingTransport{Respond: func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "Bearer tok-1" {
			resp := integrationtest.Response(http.StatusUnauthorized, "expired")
			respBody = resp.Body.(*integrationtest.Body)
			return resp, nil
		}
		return integrationtest.Response(http.StatusOK, ""), nil
	}}
	rt := transport.Chain(base, transport.Auth(src))

	// First attempt: rejected, token invalidated, 401 handed back intact.
	req, _ := authRequest(t, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first attempt = %v, %v; want the 401", resp, err)
	}
	if respBody.Closed() {
		t.Error("401 body closed; it belongs to the caller")
	}
	resp.Body.Close()
	if n := src.invalidations(); n != 1 {
		t.Fatalf("Invalidate called %d times, want 1", n)
	}

	// Next attempt (as Retry would make): fresh token, success.
	req, _ = authRequest(t, nil)
	resp, err = rt.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("second attempt = %v, %v; want 200", resp, err)
	}
	resp.Body.Close()
	if got := base.Requests()[1].Header.Get("Authorization"); got != "Bearer tok-2" {
		t.Errorf("second attempt sent %q, want Bearer tok-2", got)
	}
	if n := src.invalidations(); n != 1 {
		t.Errorf("Invalidate called %d times in total, want 1", n)
	}
}

func TestAuth_Late401KeepsNewerToken(t *testing.T) {
	src := &rotatingSource{}
	base := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		// Another request refreshed the token while this one was in flight.
		src.Invalidate()
		return integrationtest.Response(http.StatusUnauthorized, ""), nil
	}}
	req, _ := authRequest(t, nil)
	resp, err := transport.Chain(base, transport.Auth(src)).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := src.invalidations(); n != 1 {
		t.Errorf("Invalidate called %d times, want 1 (only the concurrent refresh)", n)
	}
}

func TestAuth_401WithoutInvalidator(t *testing.T) {
	base := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		return integrationtest.Response(http.StatusUnauthorized, ""), nil
	}}
	req, _ := authRequest(t, nil)
	resp, err := transport.Chain(base, transport.Auth(auth.StaticToken("t"))).RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("RoundTrip() = %v, %v", resp, err)
	}
	resp.Body.Close()
}

func TestAuth_ConcurrentRace(t *testing.T) {
	src := &rotatingSource{}
	var n sync.Mutex
	calls := 0
	base := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		n.Lock()
		defer n.Unlock()
		calls++
		if calls%3 == 0 {
			return integrationtest.Response(http.StatusUnauthorized, ""), nil
		}
		return integrationtest.Response(http.StatusOK, ""), nil
	}}
	rt := transport.Chain(base, transport.Auth(src))
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			req, _ := authRequest(t, nil)
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
