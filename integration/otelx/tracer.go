// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

// Package otelx adapts OpenTelemetry to the integration layer. It is the
// only package of the layer that imports OpenTelemetry, so the core stays
// dependency-free; use it with integration.WithTracer(otelx.NewTracer()).
package otelx

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/tatenadev/x/integration/transport"
)

// instrumentationName identifies this package as the span producer.
const instrumentationName = "github.com/tatenadev/x/integration/otelx"

// Tracer implements transport.Tracer and transport.AttemptTracer with
// OpenTelemetry: one client span per logical call, following the HTTP
// client semantic conventions, with one "http.attempt" event per attempt.
// It is safe for concurrent use.
type Tracer struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

var (
	_ transport.Tracer        = (*Tracer)(nil)
	_ transport.AttemptTracer = (*Tracer)(nil)
)

// Option configures [NewTracer].
type Option func(*options)

type options struct {
	provider   trace.TracerProvider
	propagator propagation.TextMapPropagator
}

// WithTracerProvider sets the provider spans come from. The default is the
// global provider (otel.GetTracerProvider).
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.provider = tp }
}

// WithPropagator sets how span context is written into request headers.
// The default is the global propagator (otel.GetTextMapPropagator).
func WithPropagator(p propagation.TextMapPropagator) Option {
	return func(o *options) { o.propagator = p }
}

// NewTracer returns a Tracer. With no options it uses the global provider
// and propagator, as configured by the application's OTel setup.
func NewTracer(opts ...Option) *Tracer {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.provider == nil {
		o.provider = otel.GetTracerProvider()
	}
	if o.propagator == nil {
		o.propagator = otel.GetTextMapPropagator()
	}
	return &Tracer{tracer: o.provider.Tracer(instrumentationName), propagator: o.propagator}
}

// Start begins a client span named "<method> <route>". The route is the
// templated one, never the raw path, and url.full is redacted.
func (t *Tracer) Start(ctx context.Context, req *http.Request) (context.Context, func(*http.Response, error)) {
	route := transport.Route(req)
	attrs := append(methodAttrs(req.Method),
		semconv.URLFull(transport.RedactURL(req.URL)),
		semconv.URLTemplate(route),
	)
	attrs = append(attrs, serverAttrs(req)...)

	ctx, span := t.tracer.Start(ctx, req.Method+" "+route,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...))

	return ctx, func(resp *http.Response, err error) {
		defer span.End()
		if err != nil {
			span.RecordError(err)
			span.SetAttributes(semconv.ErrorType(err))
			span.SetStatus(codes.Error, err.Error())
			return
		}
		span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
		// Client spans treat every 4xx and 5xx as an error (semconv).
		if resp.StatusCode >= http.StatusBadRequest {
			span.SetAttributes(semconv.ErrorTypeKey.String(strconv.Itoa(resp.StatusCode)))
			span.SetStatus(codes.Error, "")
		}
	}
}

// Inject writes the span context in ctx into h with the propagator.
func (t *Tracer) Inject(ctx context.Context, h http.Header) {
	t.propagator.Inject(ctx, propagation.HeaderCarrier(h))
}

// TraceAttempt adds an "http.attempt" event to the call's span.
func (t *Tracer) TraceAttempt(ctx context.Context, a transport.AttemptInfo) {
	attrs := []attribute.KeyValue{
		attribute.Int("http.attempt", a.Attempt),
		attribute.Int64("http.attempt.duration_ms", a.Duration.Milliseconds()),
		attribute.Bool("http.attempt.retry", a.Retry),
	}
	if a.StatusCode != 0 {
		attrs = append(attrs, semconv.HTTPResponseStatusCode(a.StatusCode))
	}
	if a.Retry {
		attrs = append(attrs, attribute.Int64("http.attempt.delay_ms", a.Delay.Milliseconds()))
	}
	if a.Err != nil {
		attrs = append(attrs, semconv.ErrorType(a.Err), attribute.String("exception.message", a.Err.Error()))
	}
	trace.SpanFromContext(ctx).AddEvent("http.attempt", trace.WithAttributes(attrs...))
}

// methodAttrs follows semconv: known methods as-is, anything else as
// "_OTHER" with the original kept separately.
func methodAttrs(method string) []attribute.KeyValue {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return []attribute.KeyValue{semconv.HTTPRequestMethodKey.String(method)}
	}
	return []attribute.KeyValue{semconv.HTTPRequestMethodOther, semconv.HTTPRequestMethodOriginal(method)}
}

func serverAttrs(req *http.Request) []attribute.KeyValue {
	host, portStr, err := net.SplitHostPort(req.URL.Host)
	if err != nil {
		host = req.URL.Host
		switch req.URL.Scheme {
		case "https":
			portStr = "443"
		case "http":
			portStr = "80"
		}
	}
	attrs := []attribute.KeyValue{semconv.ServerAddress(host)}
	if port, err := strconv.Atoi(portStr); err == nil {
		attrs = append(attrs, semconv.ServerPort(port))
	}
	return attrs
}
