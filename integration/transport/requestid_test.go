// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport_test

import (
	"context"
	"maps"
	"net/http"
	"testing"
	"uuid"

	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/transport"
)

func TestRequestID(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		opts    []transport.RequestIDOption
		header  http.Header
		ctx     func(context.Context) context.Context
		wantID  string // "" = any valid UUID
		wantKey bool
		keepKey string
	}{
		{name: "generated", method: http.MethodGet},
		{name: "header kept", method: http.MethodGet, header: http.Header{"X-Request-Id": {"caller-id"}}, wantID: "caller-id"},
		{name: "from context", method: http.MethodGet, wantID: "ctx-id",
			ctx: func(ctx context.Context) context.Context { return transport.WithRequestID(ctx, "ctx-id") }},
		{name: "POST not opted in", method: http.MethodPost},
		{name: "POST opted in for all", method: http.MethodPost, opts: []transport.RequestIDOption{transport.IdempotencyKeys()}, wantKey: true},
		{name: "PATCH opted in per call", method: http.MethodPatch, wantKey: true, ctx: transport.Idempotent},
		{name: "GET never gets a key", method: http.MethodGet, opts: []transport.RequestIDOption{transport.IdempotencyKeys()}},
		{name: "existing key kept", method: http.MethodPost, opts: []transport.RequestIDOption{transport.IdempotencyKeys()},
			header: http.Header{"Idempotency-Key": {"order-42"}}, wantKey: true, keepKey: "order-42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(ctx)
			}
			req, err := http.NewRequestWithContext(ctx, tt.method, "https://api.test/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			maps.Copy(req.Header, tt.header)
			before := req.Header.Clone()
			base := &integrationtest.RecordingTransport{}
			resp, err := transport.Chain(base, transport.RequestID(tt.opts...)).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			sent := base.Requests()[0]
			id := sent.Header.Get(transport.HeaderRequestID)
			if tt.wantID != "" && id != tt.wantID {
				t.Errorf("X-Request-ID = %q, want %q", id, tt.wantID)
			}
			if tt.wantID == "" {
				if _, err := uuid.Parse(id); err != nil {
					t.Errorf("X-Request-ID %q is not a UUID: %v", id, err)
				}
			}
			if ctxID, _ := transport.RequestIDFromContext(sent.Context()); ctxID != id {
				t.Errorf("context ID = %q, header ID = %q", ctxID, id)
			}
			key := sent.Header.Get(transport.HeaderIdempotencyKey)
			if (key != "") != tt.wantKey {
				t.Errorf("Idempotency-Key = %q, want present=%v", key, tt.wantKey)
			}
			if tt.keepKey != "" && key != tt.keepKey {
				t.Errorf("Idempotency-Key = %q, want %q", key, tt.keepKey)
			}
			if len(req.Header) != len(before) {
				t.Errorf("caller's request mutated: %v, was %v", req.Header, before)
			}
		})
	}
}
