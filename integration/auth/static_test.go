// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package auth_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tatenadev/x/integration/auth"
)

func TestStaticSources(t *testing.T) {
	tests := []struct {
		name      string
		src       auth.TokenSource
		wantName  string
		wantValue string
	}{
		{name: "bearer", src: auth.StaticToken("s3cret"), wantName: "Authorization", wantValue: "Bearer s3cret"},
		{name: "api key", src: auth.APIKey("x-api-key", "s3cret"), wantName: "X-Api-Key", wantValue: "s3cret"},
		{name: "basic", src: auth.BasicAuth("user", "s3cret"), wantName: "Authorization", wantValue: "Basic dXNlcjpzM2NyZXQ="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := tt.src.Token(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			name, value := tok.Header()
			if name != tt.wantName || value != tt.wantValue {
				t.Errorf("Header() = %q, %q; want %q, %q", name, value, tt.wantName, tt.wantValue)
			}
		})
	}
}

func TestToken_HeaderSchemeCaseInsensitive(t *testing.T) {
	name, value := auth.Token{Type: "bearer", Value: "v"}.Header()
	if name != "Authorization" || value != "bearer v" {
		t.Errorf("Header() = %q, %q", name, value)
	}
}

// Secrets must not reach fmt or slog output, whatever the verb.
func TestRedaction(t *testing.T) {
	cc, err := auth.NewClientCredentials("https://idp.test/token", "client", "cl1ent-s3cret")
	if err != nil {
		t.Fatal(err)
	}
	values := []any{
		auth.Token{Type: "Bearer", Value: "s3cret", Expiry: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		auth.StaticToken("s3cret"),
		auth.BasicAuth("user", "s3cret"),
		auth.APIKey("X-Api-Key", "s3cret"),
		cc,
	}
	for _, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, "s3cret") || strings.Contains(out, "czNjcmV0") {
				t.Errorf("fmt %s of %T leaks the secret: %s", verb, v, out)
			}
		}
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "v", v)
		slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "v", v)
		if strings.Contains(buf.String(), "s3cret") || strings.Contains(buf.String(), "czNjcmV0") {
			t.Errorf("slog of %T leaks the secret: %s", v, buf.String())
		}
	}
}

func TestContextSource(t *testing.T) {
	user := auth.Token{Type: "Bearer", Value: "user"}
	service := auth.StaticToken("service")
	tests := []struct {
		name    string
		src     auth.Context
		withTok bool
		want    string
		wantErr error
	}{
		{name: "token in context", src: auth.FromContext(), withTok: true, want: "user"},
		{name: "missing", src: auth.FromContext(), wantErr: auth.ErrNoToken},
		{name: "fallback", src: auth.Context{Fallback: service}, want: "service"},
		{name: "context wins over fallback", src: auth.Context{Fallback: service}, withTok: true, want: "user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.withTok {
				ctx = auth.WithToken(ctx, user)
			}
			tok, err := tt.src.Token(ctx)
			if !errors.Is(err, tt.wantErr) || tok.Value != tt.want {
				t.Errorf("Token() = %v (%q), %v; want %q, %v", tok, tok.Value, err, tt.want, tt.wantErr)
			}
		})
	}
}
