// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/transport"
)

// logLines captures slog JSON output as decoded records.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logLines) logger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: level}))
}

func (l *logLines) raw() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logLines) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(l.raw()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func (l *logLines) withMsg(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range l.records(t) {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func TestLogging_OneLinePerCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs logLines
		base := &integrationtest.RecordingTransport{Respond: integrationtest.Script(
			integrationtest.Status(503, ""), integrationtest.Status(502, ""), integrationtest.Status(200, ""))}
		rt := transport.Chain(base,
			transport.RequestID(),
			transport.Logging(logs.logger(slog.LevelDebug), "billing"),
			transport.Retry(defaultRetry(3)))
		req := newReq(t, t.Context(), http.MethodGet, nil)
		req.URL.Path = "/invoices/12345"
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		calls := logs.withMsg(t, "integration call")
		if len(calls) != 1 {
			t.Fatalf("%d call lines, want 1", len(calls))
		}
		c := calls[0]
		sentID := base.Requests()[0].Header.Get(transport.HeaderRequestID)
		want := map[string]any{"level": "INFO", "service": "billing", "method": "GET", "route": "/invoices/{id}",
			"status": float64(200), "attempts": float64(3), "request_id": sentID, "url": "https://api.test/invoices/12345"}
		for k, v := range want {
			if c[k] != v {
				t.Errorf("%s = %v, want %v", k, c[k], v)
			}
		}
		if _, ok := c["duration"]; !ok {
			t.Error("no duration")
		}
		attempts := logs.withMsg(t, "integration attempt")
		if len(attempts) != 3 {
			t.Fatalf("%d attempt lines, want 3", len(attempts))
		}
		for i, a := range attempts {
			if a["level"] != "DEBUG" || a["attempt"] != float64(i+1) || a["retry"] != (i < 2) {
				t.Errorf("attempt line %d = %v", i+1, a)
			}
		}
	})
}

func TestLogging_Levels(t *testing.T) {
	tests := []struct {
		name    string
		respond integrationtest.RespondFunc
		level   string
	}{
		{name: "2xx", respond: integrationtest.Status(200, ""), level: "INFO"},
		{name: "4xx", respond: integrationtest.Status(404, ""), level: "INFO"},
		{name: "5xx", respond: integrationtest.Status(500, ""), level: "WARN"},
		{name: "transport error", respond: integrationtest.Fail(errors.New("reset")), level: "WARN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs logLines
			base := &integrationtest.RecordingTransport{Respond: tt.respond}
			resp, err := transport.Chain(base, transport.Logging(logs.logger(slog.LevelInfo), "s")).
				RoundTrip(newReq(t, t.Context(), http.MethodGet, nil))
			if err == nil {
				resp.Body.Close()
			}
			recs := logs.records(t)
			if len(recs) != 1 || recs[0]["level"] != tt.level {
				t.Fatalf("records = %v, want one %s line", recs, tt.level)
			}
			if (err != nil) != (recs[0]["error"] != nil) {
				t.Errorf("error attr = %v for err %v", recs[0]["error"], err)
			}
		})
	}
}

func TestLogging_Redaction(t *testing.T) {
	const secret = "s3cret"
	var logs logLines
	base := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		resp := integrationtest.Response(http.StatusOK, `{"access_token":"`+secret+`","expires_in":60}`)
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Set("Set-Cookie", "session="+secret)
		return resp, nil
	}}
	rt := transport.Chain(base,
		transport.Logging(logs.logger(slog.LevelDebug), "s", transport.LogHeaders(), transport.LogBodies()),
		transport.Auth(auth.StaticToken(secret)))

	body := `{"user":"ann","password":"` + secret + `"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://user:"+secret+"@api.test/login?api_key="+secret+"&page=2", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "session="+secret)
	req.Header.Set("X-Api-Key", secret)
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if out := logs.raw(); strings.Contains(out, secret) {
		t.Errorf("log output leaks a secret:\n%s", out)
	}
	c := logs.withMsg(t, "integration call")[0]
	if c["url"] != "https://REDACTED@api.test/login?api_key=REDACTED&page=2" {
		t.Errorf("url = %v", c["url"])
	}
	if !strings.Contains(c["request_body"].(string), `"user":"ann"`) || !strings.Contains(c["response_body"].(string), `"expires_in":60`) {
		t.Errorf("bodies not logged usefully: %v / %v", c["request_body"], c["response_body"])
	}
	if !strings.Contains(string(got), secret) {
		t.Error("caller's response body was altered by body logging")
	}
	if base.Requests()[0].Header.Get("Authorization") != "Bearer "+secret {
		t.Error("redaction must not touch what is sent")
	}
}

func TestLogging_LargeBodyIntact(t *testing.T) {
	var logs logLines
	big := `{"items":[` + strings.Repeat(`"x",`, 5000) + `"x"]}`
	base := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		resp := integrationtest.Response(http.StatusOK, big)
		resp.Header.Set("Content-Type", "application/json")
		return resp, nil
	}}
	resp, err := transport.Chain(base, transport.Logging(logs.logger(slog.LevelInfo), "s", transport.LogBodies())).
		RoundTrip(newReq(t, t.Context(), http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != big {
		t.Errorf("caller read %d bytes, want %d", len(got), len(big))
	}
	logged := logs.withMsg(t, "integration call")[0]["response_body"].(string)
	if !strings.HasSuffix(logged, "…(truncated)") || len(logged) > 4<<10+64 {
		t.Errorf("logged body: %d bytes, suffix %q", len(logged), logged[max(0, len(logged)-20):])
	}
}

func TestLogging_DefaultsAndQuiet(t *testing.T) {
	var logs logLines
	base := &integrationtest.RecordingTransport{}
	req := newReq(t, t.Context(), http.MethodPut, []byte(`{"password":"x"}`))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := transport.Chain(base, transport.Logging(logs.logger(slog.LevelInfo), "s"), transport.Retry(defaultRetry(1))).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	c := logs.records(t)
	if len(c) != 1 {
		t.Fatalf("got %d lines at Info, want 1 (no Debug attempt lines)", len(c))
	}
	for _, k := range []string{"request_headers", "response_headers", "request_body", "response_body"} {
		if _, ok := c[0][k]; ok {
			t.Errorf("%s logged by default", k)
		}
	}
	if len(req.Header) != 1 || req.Header.Get("Authorization") != "Bearer x" {
		t.Error("Logging mutated the caller's request")
	}
}

type metricsSink struct {
	mu   sync.Mutex
	recs []transport.RequestInfo
}

func (m *metricsSink) RecordRequest(_ context.Context, info transport.RequestInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, info)
}

func TestMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := &metricsSink{}
		base := &integrationtest.RecordingTransport{Respond: integrationtest.Script(
			integrationtest.Status(503, ""), integrationtest.Status(404, ""))}
		rt := transport.Chain(base, transport.Metrics(sink, "billing"), transport.Retry(defaultRetry(3)))
		req := newReq(t, t.Context(), http.MethodGet, nil)
		req.URL.Path, req.URL.RawQuery = "/customers/987654/invoices", "token=s3cret"
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if len(sink.recs) != 1 {
			t.Fatalf("%d records, want 1", len(sink.recs))
		}
		r := sink.recs[0]
		if r.Service != "billing" || r.Method != "GET" || r.Route != "/customers/{id}/invoices" ||
			r.StatusCode != 404 || r.Attempts != 2 || r.Err != nil {
			t.Errorf("record = %+v", r)
		}
		if strings.Contains(r.Route, "987654") || strings.Contains(r.Route, "s3cret") || strings.Contains(r.Route, "?") {
			t.Errorf("route %q carries raw URL parts", r.Route)
		}
	})
}

// fakeTracer records spans and propagates a fixed header.
type fakeTracer struct {
	mu       sync.Mutex
	started  int
	finished []string
	events   []transport.AttemptInfo
}

type spanKey struct{}

func (f *fakeTracer) Start(ctx context.Context, req *http.Request) (context.Context, func(*http.Response, error)) {
	f.mu.Lock()
	f.started++
	f.mu.Unlock()
	return context.WithValue(ctx, spanKey{}, "span-1"), func(resp *http.Response, err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err != nil {
			f.finished = append(f.finished, "error")
			return
		}
		f.finished = append(f.finished, http.StatusText(resp.StatusCode))
	}
}

func (f *fakeTracer) Inject(ctx context.Context, h http.Header) {
	h.Set("Traceparent", ctx.Value(spanKey{}).(string))
}

func (f *fakeTracer) TraceAttempt(ctx context.Context, a transport.AttemptInfo) {
	if ctx.Value(spanKey{}) != "span-1" {
		panic("attempt traced outside the span")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, a)
}

func TestTracing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &fakeTracer{}
		var spanInside []any
		probe := func(next http.RoundTripper) http.RoundTripper {
			return transport.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				spanInside = append(spanInside, req.Context().Value(spanKey{}))
				return next.RoundTrip(req)
			})
		}
		base := &integrationtest.RecordingTransport{Respond: integrationtest.Script(
			integrationtest.Status(503, ""), integrationtest.Status(200, ""))}
		rt := transport.Chain(base, transport.Tracing(tr), transport.RequestID(), probe, transport.Retry(defaultRetry(3)))
		req := newReq(t, t.Context(), http.MethodGet, nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if tr.started != 1 || len(tr.finished) != 1 || tr.finished[0] != "OK" {
			t.Errorf("spans: started %d, finished %v; want one span ending with the final 200", tr.started, tr.finished)
		}
		for i, r := range base.Requests() {
			if r.Header.Get("Traceparent") != "span-1" {
				t.Errorf("attempt %d missing the injected header", i+1)
			}
		}
		if len(tr.events) != 2 || tr.events[0].StatusCode != 503 || !tr.events[0].Retry || tr.events[1].StatusCode != 200 {
			t.Errorf("attempt events = %+v", tr.events)
		}
		if len(spanInside) != 1 || spanInside[0] != "span-1" {
			t.Errorf("downstream middleware saw span %v", spanInside)
		}
		if req.Header.Get("Traceparent") != "" {
			t.Error("Tracing mutated the caller's request")
		}
	})
}

func TestTracing_FinishOnError(t *testing.T) {
	tr := &fakeTracer{}
	base := &integrationtest.RecordingTransport{Respond: integrationtest.Fail(errors.New("reset"))}
	_, err := transport.Chain(base, transport.Tracing(tr)).RoundTrip(newReq(t, t.Context(), http.MethodGet, nil))
	if err == nil || len(tr.finished) != 1 || tr.finished[0] != "error" {
		t.Errorf("finished = %v for err %v", tr.finished, err)
	}
}
