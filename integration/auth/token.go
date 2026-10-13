// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

// Package auth supplies credentials to the integration layer's auth
// transport. Domain code never sets Authorization itself; it hands the
// client a [TokenSource].
//
// Token values never appear in fmt or log/slog output: [Token] and the
// sources here format with the value redacted.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Token is one credential and how to send it.
type Token struct {
	// Type is "Bearer" or "Basic" (case-insensitive), sent as
	// "Authorization: <Type> <Value>". Any other Type is a header name, such
	// as "X-Api-Key", sent as "<Type>: <Value>".
	Type   string
	Value  string
	Expiry time.Time // zero = never expires
}

// Header returns the header name and value that carry t.
func (t Token) Header() (name, value string) {
	if t.isScheme() {
		return "Authorization", t.Type + " " + t.Value
	}
	return http.CanonicalHeaderKey(t.Type), t.Value
}

func (t Token) isScheme() bool {
	return strings.EqualFold(t.Type, "Bearer") || strings.EqualFold(t.Type, "Basic")
}

// String formats t without its value.
func (t Token) String() string {
	s := t.Type + " REDACTED"
	if !t.Expiry.IsZero() {
		s += " (expires " + t.Expiry.Format(time.RFC3339) + ")"
	}
	return s
}

// GoString formats t for %#v without its value.
func (t Token) GoString() string { return "auth.Token{" + t.String() + "}" }

// LogValue logs t without its value.
func (t Token) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("type", t.Type), slog.String("value", "REDACTED")}
	if !t.Expiry.IsZero() {
		attrs = append(attrs, slog.Time("expiry", t.Expiry))
	}
	return slog.GroupValue(attrs...)
}

// TokenSource supplies the token for a request. Implementations must be
// safe for concurrent use.
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
}

// Invalidator is optionally implemented by caching sources so the auth
// transport can force a refresh after a 401.
type Invalidator interface {
	Invalidate()
}

// TokenSourceFunc adapts a function to [TokenSource].
type TokenSourceFunc func(ctx context.Context) (Token, error)

// Token calls f(ctx).
func (f TokenSourceFunc) Token(ctx context.Context) (Token, error) { return f(ctx) }
