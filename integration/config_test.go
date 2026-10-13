// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"errors"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tatenadev/x/integration"
)

func TestNew_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		service string
		baseURL string
		opts    []integration.Option
	}{
		{name: "empty service", service: "", baseURL: "https://api.test"},
		{name: "blank service", service: "  ", baseURL: "https://api.test"},
		{name: "unparsable base URL", service: "s", baseURL: "https://api.test:port"},
		{name: "non-http scheme", service: "s", baseURL: "ftp://api.test"},
		{name: "relative base URL", service: "s", baseURL: "/api"},
		{name: "no host", service: "s", baseURL: "https://"},
		{name: "userinfo", service: "s", baseURL: "https://user:pass@api.test"},
		{name: "zero timeout", service: "s", baseURL: "https://api.test",
			opts: []integration.Option{integration.WithTimeout(0)}},
		{name: "negative timeout", service: "s", baseURL: "https://api.test",
			opts: []integration.Option{integration.WithTimeout(-time.Second)}},
		{name: "attempt timeout above timeout", service: "s", baseURL: "https://api.test",
			opts: []integration.Option{integration.WithTimeout(time.Second), integration.WithAttemptTimeout(2 * time.Second)}},
		{name: "max attempts 0", service: "s", baseURL: "https://api.test",
			opts: []integration.Option{integration.WithRetry(0, time.Millisecond, time.Second)}},
		{name: "backoff max below base", service: "s", baseURL: "https://api.test",
			opts: []integration.Option{integration.WithRetry(3, time.Second, time.Millisecond)}},
		{name: "nil base transport", service: "s", baseURL: "https://api.test",
			opts: []integration.Option{integration.WithBaseTransport(nil)}},
		{name: "nil middleware", service: "s", baseURL: "https://api.test",
			opts: []integration.Option{integration.WithMiddleware(nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := integration.New(tt.service, tt.baseURL, tt.opts...)
			if !errors.Is(err, integration.ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
			if c != nil {
				t.Errorf("New() client = %v, want nil", c)
			}
		})
	}
}

func TestNew_ParseErrorHidesBaseURL(t *testing.T) {
	_, err := integration.New("s", "https://user:s3cret@api.test:port")
	if err == nil {
		t.Fatal("New() error = nil")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error %q leaks the base URL password", err)
	}
}

func TestNew_Valid(t *testing.T) {
	tests := []struct {
		name string
		opts []integration.Option
	}{
		{name: "defaults"},
		{name: "short timeout lowers default attempt timeout",
			opts: []integration.Option{integration.WithTimeout(time.Second)}},
		{name: "single attempt",
			opts: []integration.Option{integration.WithRetry(1, time.Millisecond, time.Millisecond)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := integration.New("billing", "https://api.test/v1", tt.opts...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if c == nil {
				t.Fatal("New() client = nil")
			}
		})
	}
}

func validConfig() integration.Config {
	return integration.Config{
		Service:         "billing",
		BaseURL:         &url.URL{Scheme: "https", Host: "api.test"},
		Timeout:         time.Second,
		AttemptTimeout:  time.Second,
		MaxAttempts:     1,
		BackoffBase:     time.Millisecond,
		BackoffMax:      time.Millisecond,
		BreakerFailures: 1,
		BreakerCooldown: time.Second,
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*integration.Config)
		wantErr string // substring; "" means valid
	}{
		{name: "valid", mutate: func(*integration.Config) {}},
		{name: "rate limited", mutate: func(c *integration.Config) { c.RateLimit, c.RateBurst = 10, 5 }},
		{name: "nil base URL", mutate: func(c *integration.Config) { c.BaseURL = nil }, wantErr: "base URL is missing"},
		{name: "breaker failures", mutate: func(c *integration.Config) { c.BreakerFailures = 0 }, wantErr: "breaker failures"},
		{name: "breaker cooldown", mutate: func(c *integration.Config) { c.BreakerCooldown = 0 }, wantErr: "breaker cooldown"},
		{name: "backoff base", mutate: func(c *integration.Config) { c.BackoffBase = 0 }, wantErr: "backoff base"},
		{name: "negative rate", mutate: func(c *integration.Config) { c.RateLimit = -1 }, wantErr: "rate limit"},
		{name: "NaN rate", mutate: func(c *integration.Config) { c.RateLimit = math.NaN() }, wantErr: "rate limit"},
		{name: "rate without burst", mutate: func(c *integration.Config) { c.RateLimit = 1 }, wantErr: "rate burst"},
		{name: "negative burst", mutate: func(c *integration.Config) { c.RateBurst = -1 }, wantErr: "rate burst"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if !errors.Is(err, integration.ErrInvalidConfig) {
				t.Fatalf("Validate() error = %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidate_ReportsAllProblems(t *testing.T) {
	err := integration.Config{}.Validate()
	for _, want := range []string{"service", "base URL", "timeout", "max attempts", "backoff", "breaker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error %q does not mention %q", err, want)
		}
	}
}
