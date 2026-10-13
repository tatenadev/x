// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/transport"
)

// newClient builds a client over rt. It makes a single attempt unless opts
// say otherwise, so tests of non-retry behavior never wait on backoff.
func newClient(t *testing.T, baseURL string, rt http.RoundTripper, opts ...integration.Option) *integration.Client {
	t.Helper()
	base := []integration.Option{
		integration.WithBaseTransport(rt),
		integration.WithRetry(1, time.Millisecond, time.Millisecond),
	}
	c, err := integration.New("billing", baseURL, append(base, opts...)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

func newRequest(t *testing.T, method, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestDelete_URL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		path    string
		want    string
	}{
		{name: "base path kept", baseURL: "https://api.test/v1", path: "/invoices/42", want: "https://api.test/v1/invoices/42"},
		{name: "base trailing slash", baseURL: "https://api.test/v1/", path: "invoices/42", want: "https://api.test/v1/invoices/42"},
		{name: "no base path", baseURL: "https://api.test", path: "/x", want: "https://api.test/x"},
		{name: "empty path", baseURL: "https://api.test/v1", path: "", want: "https://api.test/v1"},
		{name: "escaped segment stays escaped", baseURL: "https://api.test/v1", path: "/invoices/" + url.PathEscape("a/b c"),
			want: "https://api.test/v1/invoices/a%2Fb%20c"},
		{name: "queries merge", baseURL: "https://api.test/v1?api-version=2", path: "/x?limit=5",
			want: "https://api.test/v1/x?api-version=2&limit=5"},
		{name: "path query overrides base", baseURL: "https://api.test/v1?api-version=2", path: "/x?api-version=3",
			want: "https://api.test/v1/x?api-version=3"},
		{name: "base query without path query", baseURL: "https://api.test/v1?api-version=2", path: "/x",
			want: "https://api.test/v1/x?api-version=2"},
		{name: "fragment dropped", baseURL: "https://api.test/v1#frag", path: "/x#y", want: "https://api.test/v1/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{}
			c := newClient(t, tt.baseURL, rt)
			if err := c.Delete(t.Context(), tt.path); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			reqs := rt.Requests()
			if len(reqs) != 1 {
				t.Fatalf("got %d requests, want 1", len(reqs))
			}
			if reqs[0].Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", reqs[0].Method)
			}
			if got := reqs[0].URL.String(); got != tt.want {
				t.Errorf("URL = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDelete_BaseURLNotMutated(t *testing.T) {
	rt := &integrationtest.RecordingTransport{}
	c := newClient(t, "https://api.test/v1?api-version=2", rt)
	for _, p := range []string{"?x=1", "/a?x=1", "/b"} {
		if err := c.Delete(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	reqs := rt.Requests()
	if got, want := reqs[2].URL.String(), "https://api.test/v1/b?api-version=2"; got != want {
		t.Errorf("last URL = %s, want %s (base URL leaked state from an earlier call)", got, want)
	}
}

func TestDelete_ClosesBody(t *testing.T) {
	body := integrationtest.NewBody("ok")
	rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		resp := integrationtest.Response(http.StatusOK, "")
		resp.Body = body
		return resp, nil
	}}
	if err := newClient(t, "https://api.test", rt).Delete(t.Context(), "/x"); err != nil {
		t.Fatal(err)
	}
	if !body.Closed() {
		t.Error("Delete did not close the response body")
	}
}

func TestDelete_RejectsPath(t *testing.T) {
	tests := []string{
		"https://evil.test/x",
		"//evil.test/x",
		"/a/../b",
		"/..",
		"./x",
		"/a/%2e%2e/b",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{}
			err := newClient(t, "https://api.test/v1", rt).Delete(t.Context(), path)
			if err == nil {
				t.Fatal("Delete() error = nil")
			}
			if n := len(rt.Requests()); n != 0 {
				t.Errorf("sent %d requests, want 0", n)
			}
		})
	}
}

func TestDo_Success(t *testing.T) {
	var body *integrationtest.Body
	rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		resp := integrationtest.Response(http.StatusCreated, "made")
		body = resp.Body.(*integrationtest.Body)
		return resp, nil
	}}
	c := newClient(t, "https://api.test", rt)
	resp, err := c.Do(t.Context(), newRequest(t, http.MethodGet, "https://api.test/x"))
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if body.Closed() {
		t.Error("Do closed a 2xx body the caller owns")
	}
}

func TestDo_StatusError(t *testing.T) {
	tests := []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusConflict, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, false},
		{http.StatusNotImplemented, false},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var body *integrationtest.Body
			rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
				resp := integrationtest.Response(tt.status, `{"error":"nope"}`)
				body = resp.Body.(*integrationtest.Body)
				return resp, nil
			}}
			c := newClient(t, "https://api.test", rt)
			resp, err := c.Do(t.Context(), newRequest(t, http.MethodGet, "https://api.test/x"))
			if resp != nil {
				t.Errorf("Do() resp = %v, want nil", resp)
			}
			e, ok := errors.AsType[*integration.Error](err)
			if !ok {
				t.Fatalf("Do() error = %v, want *integration.Error", err)
			}
			if e.StatusCode != tt.status || e.Retryable != tt.retryable || e.Service != "billing" ||
				e.Method != http.MethodGet || e.URL != "https://api.test/x" || string(e.Body) != `{"error":"nope"}` {
				t.Errorf("Error = %+v", e)
			}
			if got := integration.IsRetryable(err); got != tt.retryable {
				t.Errorf("IsRetryable() = %v, want %v", got, tt.retryable)
			}
			if !body.Closed() {
				t.Error("error body not closed")
			}
		})
	}
}

func TestDo_ErrorBodyCapped(t *testing.T) {
	rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		return integrationtest.Response(http.StatusBadRequest, strings.Repeat("x", 10<<10)), nil
	}}
	_, err := newClient(t, "https://api.test", rt).Do(t.Context(), newRequest(t, http.MethodGet, "https://api.test/x"))
	e, ok := errors.AsType[*integration.Error](err)
	if !ok {
		t.Fatalf("Do() error = %v, want *integration.Error", err)
	}
	if len(e.Body) != 4<<10 {
		t.Errorf("len(Body) = %d, want %d", len(e.Body), 4<<10)
	}
}

func TestDo_RedactsURL(t *testing.T) {
	tests := []struct {
		name    string
		respond integrationtest.RespondFunc
	}{
		{name: "status error", respond: func(*http.Request) (*http.Response, error) {
			return integrationtest.Response(http.StatusForbidden, ""), nil
		}},
		{name: "transport error", respond: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{Respond: tt.respond}
			req := newRequest(t, http.MethodGet, "https://api.test/x?access_token=s3cret&page=2&API_KEY=k3y")
			_, err := newClient(t, "https://api.test", rt).Do(t.Context(), req)
			e, ok := errors.AsType[*integration.Error](err)
			if !ok {
				t.Fatalf("Do() error = %v, want *integration.Error", err)
			}
			for _, s := range []string{e.URL, err.Error()} {
				if strings.Contains(s, "s3cret") || strings.Contains(s, "k3y") {
					t.Errorf("%q leaks a secret", s)
				}
			}
			if !strings.Contains(e.URL, "page=2") || !strings.Contains(e.URL, "access_token=REDACTED") {
				t.Errorf("URL = %q, want page kept and token redacted", e.URL)
			}
			if got := rt.Requests()[0].URL.Query().Get("access_token"); got != "s3cret" {
				t.Errorf("sent access_token = %q; redaction must not touch the request", got)
			}
		})
	}
}

func TestDo_TransportError(t *testing.T) {
	cause := errors.New("connection reset")
	rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		return nil, cause
	}}
	_, err := newClient(t, "https://api.test", rt).Do(t.Context(), newRequest(t, http.MethodGet, "https://api.test/x"))
	e, ok := errors.AsType[*integration.Error](err)
	if !ok {
		t.Fatalf("Do() error = %v, want *integration.Error", err)
	}
	if e.StatusCode != 0 || !e.Retryable {
		t.Errorf("Error = %+v, want StatusCode 0 and retryable", e)
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause) = false for %v", err)
	}
	if _, isURLErr := errors.AsType[*url.Error](err); isURLErr {
		t.Error("error still wraps *url.Error, which carries the unredacted URL")
	}
}

func TestDo_CircuitOpenNotRetryable(t *testing.T) {
	rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		return nil, integration.ErrCircuitOpen
	}}
	_, err := newClient(t, "https://api.test", rt).Do(t.Context(), newRequest(t, http.MethodGet, "https://api.test/x"))
	if !errors.Is(err, integration.ErrCircuitOpen) || integration.IsRetryable(err) {
		t.Errorf("Do() error = %v; want ErrCircuitOpen, not retryable", err)
	}
}

func TestDo_CallerCanceled(t *testing.T) {
	cause := errors.New("shutting down")
	ctx, cancel := context.WithCancelCause(t.Context())
	rt := &integrationtest.RecordingTransport{Respond: func(req *http.Request) (*http.Response, error) {
		cancel(cause)
		return nil, req.Context().Err()
	}}
	_, err := newClient(t, "https://api.test", rt).Do(ctx, newRequest(t, http.MethodGet, "https://api.test/x"))
	e, ok := errors.AsType[*integration.Error](err)
	if !ok {
		t.Fatalf("Do() error = %v, want *integration.Error", err)
	}
	if e.Retryable {
		t.Error("caller cancellation marked retryable")
	}
	if !errors.Is(err, cause) {
		t.Errorf("Do() error = %v, want it to wrap context.Cause", err)
	}
}

func TestDo_Origin(t *testing.T) {
	tests := []struct {
		name   string
		reqURL string
		ok     bool
	}{
		{name: "same origin", reqURL: "https://api.test/other", ok: true},
		{name: "explicit default port and case", reqURL: "https://API.test:443/x", ok: true},
		{name: "other host", reqURL: "https://evil.test/x"},
		{name: "other scheme", reqURL: "http://api.test/x"},
		{name: "other port", reqURL: "https://api.test:8443/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{}
			body := integrationtest.NewBody("payload")
			req := newRequest(t, http.MethodPut, tt.reqURL)
			req.Body = body
			resp, err := newClient(t, "https://api.test/v1", rt).Do(t.Context(), req)
			if tt.ok {
				if err != nil {
					t.Fatalf("Do() error = %v", err)
				}
				resp.Body.Close()
				return
			}
			if err == nil {
				t.Fatal("Do() error = nil")
			}
			if n := len(rt.Requests()); n != 0 {
				t.Errorf("sent %d requests, want 0", n)
			}
			if !body.Closed() {
				t.Error("rejected request body not closed")
			}
		})
	}
}

func TestDo_RequestIDFromWireRequest(t *testing.T) {
	setID := func(next http.RoundTripper) http.RoundTripper {
		return transport.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			req = req.Clone(req.Context())
			req.Header.Set("X-Request-ID", "req-123")
			return next.RoundTrip(req)
		})
	}
	rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		return integrationtest.Response(http.StatusServiceUnavailable, ""), nil
	}}
	c := newClient(t, "https://api.test", rt, integration.WithMiddleware(setID))
	req := newRequest(t, http.MethodGet, "https://api.test/x")
	_, err := c.Do(t.Context(), req)
	e, ok := errors.AsType[*integration.Error](err)
	if !ok {
		t.Fatalf("Do() error = %v, want *integration.Error", err)
	}
	if e.RequestID != "req-123" {
		t.Errorf("RequestID = %q, want req-123", e.RequestID)
	}
	if req.Header.Get("X-Request-ID") != "" {
		t.Error("caller's request was mutated")
	}
}

func TestDo_Redirects(t *testing.T) {
	tests := []struct {
		name       string
		location   string
		wantStatus int // 0 = success
		wantHosts  []string
	}{
		{name: "same origin followed", location: "https://api.test/moved", wantHosts: []string{"api.test", "api.test"}},
		{name: "other origin not followed", location: "https://evil.test/steal", wantStatus: http.StatusFound,
			wantHosts: []string{"api.test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{Respond: func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/start" {
					resp := integrationtest.Response(http.StatusFound, "")
					resp.Header.Set("Location", tt.location)
					return resp, nil
				}
				return integrationtest.Response(http.StatusOK, ""), nil
			}}
			resp, err := newClient(t, "https://api.test", rt).Do(t.Context(), newRequest(t, http.MethodGet, "https://api.test/start"))
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("Do() error = %v", err)
				}
				resp.Body.Close()
			} else if e, ok := errors.AsType[*integration.Error](err); !ok || e.StatusCode != tt.wantStatus {
				t.Fatalf("Do() error = %v, want *integration.Error with status %d", err, tt.wantStatus)
			}
			var hosts []string
			for _, r := range rt.Requests() {
				hosts = append(hosts, r.URL.Host)
			}
			if strings.Join(hosts, ",") != strings.Join(tt.wantHosts, ",") {
				t.Errorf("hosts requested = %v, want %v", hosts, tt.wantHosts)
			}
		})
	}
}

func TestClient_TestServer(t *testing.T) {
	var got string
	srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.RequestURI()
		w.WriteHeader(http.StatusNoContent)
	}))

	c := newClient(t, srv.URL+"/v1", srv.Client().Transport)
	if err := c.Delete(t.Context(), "/invoices/42"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if want := "DELETE /v1/invoices/42"; got != want {
		t.Errorf("server saw %q, want %q", got, want)
	}
}

func TestClient_Timeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // hang until the client gives up
		}))

		const timeout = 2 * time.Second
		c := newClient(t, srv.URL, srv.Client().Transport, integration.WithTimeout(timeout))
		start := time.Now()
		err := c.Delete(t.Context(), "/slow")
		if elapsed := time.Since(start); elapsed != timeout {
			t.Errorf("gave up after %v, want %v", elapsed, timeout)
		}
		e, ok := errors.AsType[*integration.Error](err)
		if !ok {
			t.Fatalf("Delete() error = %v, want *integration.Error", err)
		}
		if !e.Retryable || e.StatusCode != 0 {
			t.Errorf("Error = %+v, want a retryable transport error", e)
		}
	})
}
