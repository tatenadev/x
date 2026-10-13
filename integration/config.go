// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// Defaults applied by [New] before options run.
const (
	DefaultTimeout         = 30 * time.Second
	DefaultAttemptTimeout  = 10 * time.Second
	DefaultMaxAttempts     = 3
	DefaultBackoffBase     = 100 * time.Millisecond
	DefaultBackoffMax      = 5 * time.Second
	DefaultBreakerFailures = 5
	DefaultBreakerCooldown = 30 * time.Second
	DefaultRateBurst       = 1
)

// Config is the validated, immutable configuration of a [Client]. Once a
// Config is stored in a Client it is never mutated; updates replace it.
type Config struct {
	Service         string
	BaseURL         *url.URL
	Timeout         time.Duration // whole logical call, all attempts included
	AttemptTimeout  time.Duration // one attempt; must not exceed Timeout
	MaxAttempts     int           // total attempts, including the first
	BackoffBase     time.Duration
	BackoffMax      time.Duration
	BreakerFailures int // consecutive failures that open the breaker
	BreakerCooldown time.Duration
	RateLimit       float64 // requests per second; 0 = unlimited
	RateBurst       int
}

func defaultConfig(service string, base *url.URL) Config {
	return Config{
		Service:         service,
		BaseURL:         base,
		Timeout:         DefaultTimeout,
		AttemptTimeout:  DefaultAttemptTimeout,
		MaxAttempts:     DefaultMaxAttempts,
		BackoffBase:     DefaultBackoffBase,
		BackoffMax:      DefaultBackoffMax,
		BreakerFailures: DefaultBreakerFailures,
		BreakerCooldown: DefaultBreakerCooldown,
		RateBurst:       DefaultRateBurst,
	}
}

// clone returns a copy of c that shares no mutable state with it.
func (c Config) clone() Config {
	if c.BaseURL != nil {
		c.BaseURL = c.BaseURL.Clone()
	}
	return c
}

// Validate reports every problem with c at once. The returned error wraps
// [ErrInvalidConfig].
func (c Config) Validate() error { return c.validate(true) }

// validate is Validate, with the base URL optional for [NewHTTPClient].
func (c Config) validate(requireBaseURL bool) error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if strings.TrimSpace(c.Service) == "" {
		add("service is empty")
	}
	if c.BaseURL != nil || requireBaseURL {
		if err := validateBaseURL(c.BaseURL); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Timeout <= 0 {
		add("timeout %v is not positive", c.Timeout)
	}
	if c.AttemptTimeout <= 0 {
		add("attempt timeout %v is not positive", c.AttemptTimeout)
	} else if c.Timeout > 0 && c.AttemptTimeout > c.Timeout {
		add("attempt timeout %v exceeds timeout %v", c.AttemptTimeout, c.Timeout)
	}
	if c.MaxAttempts < 1 {
		add("max attempts %d is less than 1", c.MaxAttempts)
	}
	if c.BackoffBase <= 0 {
		add("backoff base %v is not positive", c.BackoffBase)
	} else if c.BackoffMax < c.BackoffBase {
		add("backoff max %v is less than backoff base %v", c.BackoffMax, c.BackoffBase)
	}
	if c.BreakerFailures < 1 {
		add("breaker failures %d is less than 1", c.BreakerFailures)
	}
	if c.BreakerCooldown <= 0 {
		add("breaker cooldown %v is not positive", c.BreakerCooldown)
	}
	if math.IsNaN(c.RateLimit) || math.IsInf(c.RateLimit, 0) || c.RateLimit < 0 {
		add("rate limit %v is not a finite non-negative number", c.RateLimit)
	}
	if c.RateBurst < 0 || (c.RateLimit > 0 && c.RateBurst < 1) {
		add("rate burst %d is less than 1", c.RateBurst)
	}
	// The limiter waits inside the attempt, so a slot must come round
	// well within one attempt timeout.
	if c.RateLimit > 0 && c.AttemptTimeout > 0 && time.Duration(float64(time.Second)/c.RateLimit) >= c.AttemptTimeout {
		add("rate limit %v/s spaces calls %v apart, not below attempt timeout %v",
			c.RateLimit, time.Duration(float64(time.Second)/c.RateLimit), c.AttemptTimeout)
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(errs...))
}

// validateBaseURL accepts an absolute http(s) URL with a host. Userinfo is
// rejected: net/http turns it into an Authorization header, bypassing the
// auth transport.
func validateBaseURL(u *url.URL) error {
	switch {
	case u == nil:
		return errors.New("base URL is missing")
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("base URL scheme %q is not http or https", u.Scheme)
	case u.Host == "" || u.Opaque != "":
		return errors.New("base URL has no host")
	case u.User != nil:
		return errors.New("base URL must not contain userinfo; use a token source")
	}
	return nil
}
