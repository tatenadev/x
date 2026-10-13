// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
)

// ErrNoToken is returned by [FromContext] when ctx carries no token.
var ErrNoToken = errors.New("auth: no token in context")

type tokenKey struct{}

// WithToken returns a copy of ctx carrying tok, for calls made on behalf
// of a specific user or tenant.
func WithToken(ctx context.Context, tok Token) context.Context {
	return context.WithValue(ctx, tokenKey{}, tok)
}

// TokenFromContext returns the token stored by [WithToken], if any.
func TokenFromContext(ctx context.Context) (Token, bool) {
	tok, ok := ctx.Value(tokenKey{}).(Token)
	return tok, ok
}

// Context is a [TokenSource] that reads the per-request token stored with
// [WithToken]. Without one it uses Fallback, or fails with [ErrNoToken]
// when Fallback is nil.
type Context struct {
	Fallback TokenSource
}

// FromContext returns a Context source with no fallback.
func FromContext() Context { return Context{} }

// Token returns the token in ctx, or the fallback's.
func (c Context) Token(ctx context.Context) (Token, error) {
	if tok, ok := TokenFromContext(ctx); ok {
		return tok, nil
	}
	if c.Fallback != nil {
		return c.Fallback.Token(ctx)
	}
	return Token{}, ErrNoToken
}
