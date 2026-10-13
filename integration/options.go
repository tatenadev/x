// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"time"

	"github.com/tatenadev/x/integration/transport"
)

// config is what an [Option] writes to: the public [Config] plus the
// wiring that is fixed for a Client's lifetime.
type config struct {
	Config
	base       http.RoundTripper
	middleware []transport.Middleware

	// attemptTimeoutSet records an explicit WithAttemptTimeout. Without
	// one, New lowers the default attempt timeout to fit Timeout.
	attemptTimeoutSet bool
}

// Option configures a [Client] built by [New]. Options only record values;
// [Config.Validate] checks the result once all options have run.
type Option func(*config) error

// WithTimeout sets the timeout for one logical call, all attempts included.
func WithTimeout(d time.Duration) Option {
	return func(c *config) error {
		c.Timeout = d
		return nil
	}
}

// WithAttemptTimeout sets the timeout for a single attempt. It must not
// exceed the call timeout. When unset, the default (10s) is lowered to the
// call timeout if that is shorter.
func WithAttemptTimeout(d time.Duration) Option {
	return func(c *config) error {
		c.AttemptTimeout = d
		c.attemptTimeoutSet = true
		return nil
	}
}

// WithRetry sets the total number of attempts and the backoff bounds.
func WithRetry(maxAttempts int, base, max time.Duration) Option {
	return func(c *config) error {
		c.MaxAttempts = maxAttempts
		c.BackoffBase = base
		c.BackoffMax = max
		return nil
	}
}

// WithBaseTransport replaces the innermost transport, which by default is
// a fresh *http.Transport. Use it for tests or custom TLS.
func WithBaseTransport(rt http.RoundTripper) Option {
	return func(c *config) error {
		if rt == nil {
			return fmt.Errorf("%w: base transport is nil", ErrInvalidConfig)
		}
		c.base = rt
		return nil
	}
}

// WithMiddleware adds caller middleware just inside Logging, in the order
// given (the first is outermost). Repeated calls append.
func WithMiddleware(mws ...transport.Middleware) Option {
	return func(c *config) error {
		for i, mw := range mws {
			if mw == nil {
				return fmt.Errorf("%w: middleware %d is nil", ErrInvalidConfig, i)
			}
		}
		c.middleware = append(c.middleware, mws...)
		return nil
	}
}
