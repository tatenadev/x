// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/base64"
	"log/slog"
)

// Static is a [TokenSource] that always returns the same token.
type Static struct {
	tok Token
}

// StaticToken returns a source for a fixed bearer token.
func StaticToken(token string) Static {
	return Static{tok: Token{Type: "Bearer", Value: token}}
}

// APIKey returns a source that sends key in the given header, for example
// APIKey("X-Api-Key", key).
func APIKey(header, key string) Static {
	return Static{tok: Token{Type: header, Value: key}}
}

// BasicAuth returns a source for HTTP Basic authentication (RFC 7617).
func BasicAuth(username, password string) Static {
	v := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return Static{tok: Token{Type: "Basic", Value: v}}
}

// Token returns the fixed token.
func (s Static) Token(context.Context) (Token, error) { return s.tok, nil }

// String formats s without the token value.
func (s Static) String() string { return "auth.Static{" + s.tok.String() + "}" }

// GoString formats s for %#v without the token value.
func (s Static) GoString() string { return s.String() }

// LogValue logs s without the token value.
func (s Static) LogValue() slog.Value { return s.tok.LogValue() }
