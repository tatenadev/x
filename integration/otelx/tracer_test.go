// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package otelx_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/otelx"
)

// Minimal recording implementations of the OTel trace API, so the tests
// need no SDK module.

type provider struct {
	embedded.TracerProvider
	mu    sync.Mutex
	spans []*span
}

func (p *provider) Tracer(string, ...trace.TracerOption) trace.Tracer { return &tracer{p: p} }

func (p *provider) recorded() []*span {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spans
}

type tracer struct {
	embedded.Tracer
	p *provider
}

func (t *tracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	s := &span{
		name:  name,
		kind:  cfg.SpanKind(),
		attrs: cfg.Attributes(),
		sc: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{0x0a, 0x0b},
			SpanID:     trace.SpanID{byte(len(t.p.spans) + 1)},
			TraceFlags: trace.FlagsSampled,
		}),
	}
	t.p.spans = append(t.p.spans, s)
	return trace.ContextWithSpan(ctx, s), s
}

type event struct {
	name  string
	attrs []attribute.KeyValue
}

type span struct {
	embedded.Span
	mu     sync.Mutex
	name   string
	kind   trace.SpanKind
	attrs  []attribute.KeyValue
	events []event
	errs   []error
	status codes.Code
	ended  int
	sc     trace.SpanContext
}

func (s *span) End(...trace.SpanEndOption) { s.mu.Lock(); s.ended++; s.mu.Unlock() }
func (s *span) AddEvent(name string, opts ...trace.EventOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := trace.NewEventConfig(opts...)
	s.events = append(s.events, event{name, cfg.Attributes()})
}
func (s *span) AddLink(trace.Link) {}
func (s *span) IsRecording() bool  { return true }
func (s *span) RecordError(err error, _ ...trace.EventOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
}
func (s *span) SpanContext() trace.SpanContext { return s.sc }
func (s *span) SetStatus(c codes.Code, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = c
}
func (s *span) SetName(name string) { s.name = name }
func (s *span) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = append(s.attrs, kv...)
}
func (s *span) TracerProvider() trace.TracerProvider { return nil }

func (s *span) attr(key string) (attribute.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, kv := range s.attrs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func attrOf(kvs []attribute.KeyValue, key string) attribute.Value {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func newTracer(p *provider) *otelx.Tracer {
	return otelx.NewTracer(otelx.WithTracerProvider(p), otelx.WithPropagator(propagation.TraceContext{}))
}

func TestTracer_ThroughClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var traceparents []string
		var calls int
		srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			traceparents = append(traceparents, r.Header.Get("Traceparent"))
			if calls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		p := &provider{}
		c, err := integration.New("billing", srv.URL,
			integration.WithBaseTransport(srv.Client().Transport),
			integration.WithRetry(3, time.Millisecond, time.Millisecond),
			integration.WithTracer(newTracer(p)))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(integration.WithRoute(t.Context(), "/invoices/{id}"), "/invoices/42?token=s3cret"); err != nil {
			t.Fatal(err)
		}

		spans := p.recorded()
		if len(spans) != 1 {
			t.Fatalf("%d spans, want 1 per logical call", len(spans))
		}
		s := spans[0]
		if s.name != "DELETE /invoices/{id}" || s.kind != trace.SpanKindClient || s.ended != 1 || s.status != codes.Unset {
			t.Errorf("span %q kind %v ended %d status %v", s.name, s.kind, s.ended, s.status)
		}
		want := map[string]string{
			"http.request.method":       "DELETE",
			"url.template":              "/invoices/{id}",
			"http.response.status_code": "204",
			"server.address":            "example.com",
			"server.port":               "80",
		}
		for k, v := range want {
			if got, ok := s.attr(k); !ok || got.Emit() != v {
				t.Errorf("%s = %q, want %q", k, got.Emit(), v)
			}
		}
		if u, _ := s.attr("url.full"); strings.Contains(u.Emit(), "s3cret") || !strings.Contains(u.Emit(), "token=REDACTED") {
			t.Errorf("url.full = %q, want it redacted", u.Emit())
		}

		if len(s.events) != 2 {
			t.Fatalf("%d attempt events, want 2", len(s.events))
		}
		first, second := s.events[0], s.events[1]
		if first.name != "http.attempt" || attrOf(first.attrs, "http.attempt").AsInt64() != 1 ||
			!attrOf(first.attrs, "http.attempt.retry").AsBool() || attrOf(first.attrs, "http.response.status_code").AsInt64() != 503 {
			t.Errorf("first event = %+v", first)
		}
		if attrOf(second.attrs, "http.attempt").AsInt64() != 2 || attrOf(second.attrs, "http.attempt.retry").AsBool() {
			t.Errorf("second event = %+v", second)
		}

		wantTP := "00-" + s.sc.TraceID().String() + "-" + s.sc.SpanID().String() + "-01"
		for i, tp := range traceparents {
			if tp != wantTP {
				t.Errorf("attempt %d traceparent = %q, want %q", i+1, tp, wantTP)
			}
		}
	})
}

func TestTracer_Failures(t *testing.T) {
	tests := []struct {
		name      string
		respond   integrationtest.RespondFunc
		errorType string
		recorded  bool
	}{
		{name: "404", respond: integrationtest.Status(http.StatusNotFound, ""), errorType: "404"},
		{name: "transport error", respond: integrationtest.Fail(errors.New("connection reset")), recorded: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &provider{}
			c, err := integration.New("billing", "https://api.test:8443",
				integration.WithBaseTransport(&integrationtest.RecordingTransport{Respond: tt.respond}),
				integration.WithRetry(1, time.Millisecond, time.Millisecond),
				integration.WithTracer(newTracer(p)))
			if err != nil {
				t.Fatal(err)
			}
			c.Delete(t.Context(), "/x")

			s := p.recorded()[0]
			if s.status != codes.Error || s.ended != 1 {
				t.Errorf("status %v ended %d, want Error and ended once", s.status, s.ended)
			}
			et, ok := s.attr("error.type")
			if !ok || (tt.errorType != "" && et.Emit() != tt.errorType) {
				t.Errorf("error.type = %q, %v", et.Emit(), ok)
			}
			if (len(s.errs) > 0) != tt.recorded {
				t.Errorf("RecordError called %d times", len(s.errs))
			}
			if port, _ := s.attr("server.port"); port.AsInt64() != 8443 {
				t.Errorf("server.port = %v", port.Emit())
			}
		})
	}
}

func TestTracer_UnknownMethod(t *testing.T) {
	p := &provider{}
	req, err := http.NewRequestWithContext(t.Context(), "PURGE", "https://api.test/cache", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, finish := newTracer(p).Start(t.Context(), req)
	finish(integrationtest.Response(http.StatusOK, ""), nil)
	s := p.recorded()[0]
	if m, _ := s.attr("http.request.method"); m.Emit() != "_OTHER" {
		t.Errorf("http.request.method = %q, want _OTHER", m.Emit())
	}
	if m, _ := s.attr("http.request.method_original"); m.Emit() != "PURGE" {
		t.Errorf("http.request.method_original = %q", m.Emit())
	}
}

func TestTracer_GlobalDefaults(t *testing.T) {
	// The global provider and propagator are no-ops until configured; the
	// tracer must work with them all the same.
	tr := otelx.NewTracer()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := tr.Start(t.Context(), req)
	tr.Inject(ctx, req.Header)
	finish(nil, errors.New("boom"))
}
