// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/transport"
)

// config is what an [Option] writes to: the public [Config] plus the
// wiring that is fixed for a Client's lifetime.
type config struct {
	Config
	base       http.RoundTripper
	middleware []transport.Middleware
	jsonOpts   []json.Options
	tokens     auth.TokenSource
	rand       *rand.Rand
	policy     transport.RetryPolicy
	retry500   bool
	idemAll    bool
	logger     *slog.Logger
	logOpts    []transport.LoggingOption
	metrics    transport.MetricsRecorder
	tracer     transport.Tracer

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

// WithTokenSource authenticates every attempt with credentials from ts.
// The auth transport is the innermost middleware, so each retry fetches
// the token again and can pick up a refresh.
func WithTokenSource(ts auth.TokenSource) Option {
	return func(c *config) error {
		if ts == nil {
			return fmt.Errorf("%w: token source is nil", ErrInvalidConfig)
		}
		c.tokens = ts
		return nil
	}
}

// WithRand supplies the randomness for backoff jitter. Pass a seeded
// source for deterministic delays in tests. The default is the global
// source.
func WithRand(r *rand.Rand) Option {
	return func(c *config) error {
		if r == nil {
			return fmt.Errorf("%w: rand is nil", ErrInvalidConfig)
		}
		c.rand = r
		return nil
	}
}

// WithRetryPolicy replaces the default retry policy. The attempt limit,
// body replay rules and caller cancellation still apply.
func WithRetryPolicy(p transport.RetryPolicy) Option {
	return func(c *config) error {
		if p == nil {
			return fmt.Errorf("%w: retry policy is nil", ErrInvalidConfig)
		}
		c.policy = p
		return nil
	}
}

// WithRetryServerErrors makes the default policy retry a plain 500 too,
// for upstreams that use it for transient failures.
func WithRetryServerErrors() Option {
	return func(c *config) error {
		c.retry500 = true
		return nil
	}
}

// WithIdempotencyKeys gives every POST and PATCH an Idempotency-Key, which
// also makes them retryable. Use it only for upstreams that honour the
// header. To opt in a single call, use [Idempotent] on its context.
func WithIdempotencyKeys() Option {
	return func(c *config) error {
		c.idemAll = true
		return nil
	}
}

// WithRateLimit paces attempts to rps per second with bursts of up to
// burst. Every attempt, retries included, takes a token. The wait counts
// against the attempt timeout, so 1/rps must stay below it. The default is
// unlimited.
func WithRateLimit(rps float64, burst int) Option {
	return func(c *config) error {
		c.RateLimit, c.RateBurst = rps, burst
		return nil
	}
}

// WithBreaker opens the circuit after failures consecutive upstream
// failures and keeps it open for cooldown before probing again (defaults
// 5 and 30s).
func WithBreaker(failures int, cooldown time.Duration) Option {
	return func(c *config) error {
		c.BreakerFailures, c.BreakerCooldown = failures, cooldown
		return nil
	}
}

// WithLogger logs one line per call (Info, or Warn on a transport error or
// 5xx) plus a Debug line per attempt, and circuit breaker transitions.
// URLs are redacted; LogHeaders and LogBodies add redacted headers and
// bodies. Without it the client does not log.
func WithLogger(l *slog.Logger, opts ...transport.LoggingOption) Option {
	return func(c *config) error {
		if l == nil {
			return fmt.Errorf("%w: logger is nil", ErrInvalidConfig)
		}
		c.logger, c.logOpts = l, opts
		return nil
	}
}

// WithMetrics reports every call to m, with a templated route.
func WithMetrics(m transport.MetricsRecorder) Option {
	return func(c *config) error {
		if m == nil {
			return fmt.Errorf("%w: metrics recorder is nil", ErrInvalidConfig)
		}
		c.metrics = m
		return nil
	}
}

// WithTracer wraps every call in a span from t and propagates it.
func WithTracer(t transport.Tracer) Option {
	return func(c *config) error {
		if t == nil {
			return fmt.Errorf("%w: tracer is nil", ErrInvalidConfig)
		}
		c.tracer = t
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

// WithJSONOptions applies opts to every JSON encode and decode of this
// client, on top of the strict v2 defaults. Use it only for a specific
// upstream that needs leniency, and say why at the call site, e.g.
// jsontext.AllowDuplicateNames(true) for an API that repeats keys.
func WithJSONOptions(opts ...json.Options) Option {
	return func(c *config) error {
		for i, o := range opts {
			if o == nil {
				return fmt.Errorf("%w: JSON option %d is nil", ErrInvalidConfig, i)
			}
		}
		c.jsonOpts = append(c.jsonOpts, opts...)
		return nil
	}
}
