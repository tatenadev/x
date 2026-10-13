// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/tatenadev/x/integration"
)

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: io.EOF, want: false},
		{name: "retryable", err: &integration.Error{Retryable: true}, want: true},
		{name: "not retryable", err: &integration.Error{}, want: false},
		{name: "wrapped", err: fmt.Errorf("get invoice: %w", &integration.Error{Retryable: true}), want: true},
		{name: "joined", err: errors.Join(io.EOF, &integration.Error{Retryable: true}), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := integration.IsRetryable(tt.err); got != tt.want {
				t.Errorf("IsRetryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *integration.Error
		want string
	}{
		{
			name: "status",
			err: &integration.Error{Service: "billing", Method: "GET", URL: "https://api.test/x",
				StatusCode: 503, Body: []byte("not in the message")},
			want: "integration: billing: GET https://api.test/x: 503 Service Unavailable",
		},
		{
			name: "transport",
			err:  &integration.Error{Service: "billing", Method: "GET", URL: "https://api.test/x", Err: io.ErrUnexpectedEOF},
			want: "integration: billing: GET https://api.test/x: unexpected EOF",
		},
		{
			name: "unknown status",
			err:  &integration.Error{Method: "GET", URL: "https://api.test/x", StatusCode: 599},
			want: "integration: GET https://api.test/x: 599",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestError_Unwrap(t *testing.T) {
	err := fmt.Errorf("outer: %w", &integration.Error{Err: io.ErrUnexpectedEOF})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("errors.Is(%v, io.ErrUnexpectedEOF) = false", err)
	}
	e, ok := errors.AsType[*integration.Error](err)
	if !ok || e.Err != io.ErrUnexpectedEOF {
		t.Errorf("errors.AsType = %v, %v", e, ok)
	}
}
