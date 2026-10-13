// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/resilience"
)

// RetryPolicy decides whether a failed attempt is retried.
type RetryPolicy interface {
	// ShouldRetry reports whether attempt (1-based) should be retried and
	// how long to wait first. resp is nil when err != nil. Retry enforces
	// the attempt limit, body replayability, and caller cancellation
	// itself; a policy only judges the outcome.
	ShouldRetry(req *http.Request, resp *http.Response, err error, attempt int) (bool, time.Duration)
}

// DefaultPolicy retries idempotent requests (GET, HEAD, OPTIONS, PUT,
// DELETE, and POST or PATCH carrying an Idempotency-Key) after transport
// errors, attempt timeouts, and 429, 502, 503 or 504 responses, plus 500
// when RetryServerError is set. It waits per Backoff, or per Retry-After on
// 429 and 503, capped at the backoff maximum.
type DefaultPolicy struct {
	Backoff          *resilience.Backoff
	RetryServerError bool
}

// ShouldRetry implements [RetryPolicy].
func (p DefaultPolicy) ShouldRetry(req *http.Request, resp *http.Response, err error, attempt int) (bool, time.Duration) {
	if !retryableMethod(req) {
		return false, 0
	}
	if err != nil {
		return retryableError(err), p.Backoff.Delay(attempt)
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		if d, ok := resilience.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			if m := p.Backoff.Max(); m > 0 {
				d = min(d, m)
			}
			return true, d
		}
		return true, p.Backoff.Delay(attempt)
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return true, p.Backoff.Delay(attempt)
	case http.StatusInternalServerError:
		return p.RetryServerError, p.Backoff.Delay(attempt)
	}
	return false, 0
}

func retryableMethod(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	case http.MethodPost, http.MethodPatch:
		return req.Header.Get(HeaderIdempotencyKey) != ""
	}
	return false
}

// retryableError reports whether a transport error may clear up on retry.
func retryableError(err error) bool {
	switch {
	case errors.Is(err, resilience.ErrCircuitOpen),
		errors.Is(err, resilience.ErrRateLimited),
		errors.Is(err, auth.ErrNoToken):
		return false
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return false
	}
	return true
}

// RetryConfig configures [Retry].
type RetryConfig struct {
	// Policy judges each failed attempt. Nil retries nothing except a 401
	// when RetryUnauthorized is set.
	Policy RetryPolicy
	// MaxAttempts is the total number of attempts; values below 1 mean 1.
	MaxAttempts int
	// AttemptTimeout bounds each attempt, including reading its response
	// body; 0 means no per-attempt bound. An attempt that times out fails
	// with an error wrapping resilience.ErrAttemptTimeout and is retried.
	AttemptTimeout time.Duration
	// RetryUnauthorized retries one 401 per call without delay. Pair it
	// with Auth, which invalidates the rejected token, so the retry
	// carries a fresh one.
	RetryUnauthorized bool
}

// Retry returns middleware that retries failed attempts. Each attempt is a
// clone of the request with its own context (and timeout) and, after the
// first, a fresh body from GetBody; a request whose body cannot be replayed
// gets a single attempt. A response that is retried is closed. Retry stops
// when the caller's context is done, returning an error that wraps
// context.Cause, and gives up early when the next wait would outlast the
// context's deadline, returning the last outcome instead.
func Retry(cfg RetryConfig) Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return cfg.roundTrip(next, req)
		})
	}
}

func (c RetryConfig) roundTrip(next http.RoundTripper, req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	maxAttempts := max(c.MaxAttempts, 1)
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		maxAttempts = 1 // the body cannot be replayed
	}
	retried401 := false

	for attempt := 1; ; attempt++ {
		areq, cancel, err := c.newAttempt(ctx, req, attempt)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		resp, err := next.RoundTrip(areq)
		report := func(retry bool, delay time.Duration) {
			notifyAttempt(ctx, AttemptInfo{Attempt: attempt, StatusCode: statusOf(resp), Err: err,
				Duration: time.Since(started), Retry: retry, Delay: delay})
		}
		if err != nil {
			if ctx.Err() != nil {
				cancel()
				err = fmt.Errorf("retry: attempt %d: %w", attempt, context.Cause(ctx))
				report(false, 0)
				return nil, err
			}
			if errors.Is(context.Cause(areq.Context()), resilience.ErrAttemptTimeout) {
				err = fmt.Errorf("%w after %v: %w", resilience.ErrAttemptTimeout, c.AttemptTimeout, err)
			}
		}

		var retry bool
		var delay time.Duration
		if attempt < maxAttempts {
			if c.RetryUnauthorized && !retried401 && err == nil && resp.StatusCode == http.StatusUnauthorized {
				retry, retried401 = true, true
			} else if c.Policy != nil {
				retry, delay = c.Policy.ShouldRetry(req, resp, err, attempt)
			}
		}
		if retry && delay > 0 {
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
				retry = false // waiting would only end in a timeout
			}
		}

		report(retry, delay)
		if !retry {
			if err != nil {
				cancel()
				return nil, err
			}
			// The attempt context must outlive RoundTrip: it governs
			// reading the body, so release it when the body is closed.
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		if err := resilience.Sleep(ctx, delay); err != nil {
			return nil, fmt.Errorf("retry: waiting for attempt %d: %w", attempt+1, err)
		}
	}
}

// newAttempt clones req for one attempt, with its own context and, after
// the first attempt, a fresh body.
func (c RetryConfig) newAttempt(ctx context.Context, req *http.Request, attempt int) (*http.Request, context.CancelFunc, error) {
	var actx context.Context
	var cancel context.CancelFunc
	if c.AttemptTimeout > 0 {
		actx, cancel = context.WithTimeoutCause(ctx, c.AttemptTimeout, resilience.ErrAttemptTimeout)
	} else {
		actx, cancel = context.WithCancel(ctx)
	}
	areq := req.Clone(actx)
	if attempt > 1 && req.GetBody != nil && req.Body != nil && req.Body != http.NoBody {
		body, err := req.GetBody()
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("retry: replay body for attempt %d: %w", attempt, err)
		}
		areq.Body = body
	}
	return areq, cancel, nil
}

// cancelOnClose releases an attempt's context once its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
