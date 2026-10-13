// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/codec"
	"github.com/tatenadev/x/integration/resilience"
	"github.com/tatenadev/x/integration/transport"
)

// maxRedirects matches net/http's default redirect limit.
const maxRedirects = 10

// Client calls one upstream service. It is safe for concurrent use.
type Client struct {
	cfg      atomic.Pointer[Config]
	doer     Doer
	json     codec.JSON
	retry500 bool
}

// New builds a Client for service at baseURL. It applies defaults, then
// opts, and validates the result, so a misconfigured client fails here
// rather than on its first call.
func New(service, baseURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		// The *url.Error text repeats the raw URL; keep only the cause.
		return nil, fmt.Errorf("%w: base URL: %w", ErrInvalidConfig, unwrapURLError(err))
	}

	cfg, err := configure(defaultConfig(service, u), true, opts)
	if err != nil {
		return nil, err
	}

	c := &Client{json: codec.NewJSON(cfg.jsonOpts...), retry500: cfg.retry500}
	c.cfg.Store(new(cfg.Config.clone()))
	c.doer = &http.Client{
		Transport:     buildChain(cfg),
		Timeout:       cfg.Timeout,
		CheckRedirect: c.checkRedirect,
	}
	return c, nil
}

// configure applies opts over defaults, fills in derived values, and
// validates the result.
func configure(defaults Config, requireBaseURL bool, opts []Option) (*config, error) {
	cfg := &config{Config: defaults}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}
	if !cfg.attemptTimeoutSet && cfg.Timeout > 0 {
		cfg.AttemptTimeout = min(cfg.AttemptTimeout, cfg.Timeout)
	}
	if err := cfg.validate(requireBaseURL); err != nil {
		return nil, err
	}
	if cfg.base == nil {
		cfg.base = newHTTPTransport()
	}
	return cfg, nil
}

// NewHTTPClient returns an *http.Client running the same middleware chain
// as [New] (request IDs, observability, retries, rate limiting, circuit
// breaker, auth), for third-party SDKs that insist on an *http.Client and
// call more than one host. Prefer [New] for everything else: this client
// has no base URL, so it neither resolves paths, nor maps failures to
// [*Error], nor confines requests or redirects to one origin. The auth
// transport therefore sends credentials to every host the SDK (or a
// redirect) points at; use it only with SDKs for APIs you trust that much.
func NewHTTPClient(service string, opts ...Option) (*http.Client, error) {
	cfg, err := configure(defaultConfig(service, nil), false, opts)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: buildChain(cfg), Timeout: cfg.Timeout}, nil
}

// buildChain assembles the middleware stack, outermost first:
//
//	Tracing -> RequestID -> Metrics -> Logging -> caller middleware
//	  -> Retry -> RateLimit -> CircuitBreaker -> Auth -> base
//
// Tracing, Metrics and Logging are present when a tracer, recorder or
// logger is configured; RateLimit when a rate is.
func buildChain(cfg *config) http.RoundTripper {
	var ridOpts []transport.RequestIDOption
	if cfg.idemAll {
		ridOpts = append(ridOpts, transport.IdempotencyKeys())
	}
	policy := cfg.policy
	if policy == nil {
		policy = transport.DefaultPolicy{
			Backoff:          resilience.NewBackoff(cfg.BackoffBase, cfg.BackoffMax, cfg.rand),
			RetryServerError: cfg.retry500,
		}
	}

	var mws []transport.Middleware
	if cfg.tracer != nil {
		mws = append(mws, transport.Tracing(cfg.tracer))
	}
	mws = append(mws, transport.RequestID(ridOpts...))
	if cfg.metrics != nil {
		mws = append(mws, transport.Metrics(cfg.metrics, cfg.Service))
	}
	if cfg.logger != nil {
		mws = append(mws, transport.Logging(cfg.logger, cfg.Service, cfg.logOpts...))
	}
	mws = append(mws, cfg.middleware...)
	mws = append(mws, transport.Retry(transport.RetryConfig{
		Policy:            policy,
		MaxAttempts:       cfg.MaxAttempts,
		AttemptTimeout:    cfg.AttemptTimeout,
		RetryUnauthorized: canInvalidate(cfg.tokens),
	}))
	if cfg.RateLimit > 0 {
		mws = append(mws, transport.RateLimit(resilience.NewTokenBucket(cfg.RateLimit, cfg.RateBurst)))
	}
	var breakerOpts []resilience.BreakerOption
	if cfg.logger != nil {
		logger, service := cfg.logger, cfg.Service
		breakerOpts = append(breakerOpts, resilience.OnStateChange(func(from, to resilience.State) {
			level := slog.LevelInfo
			if to == resilience.Open {
				level = slog.LevelWarn
			}
			logger.LogAttrs(context.Background(), level, "integration circuit breaker",
				slog.String("service", service), slog.String("from", from.String()), slog.String("to", to.String()))
		}))
	}
	breaker := resilience.NewConsecutiveBreaker(cfg.BreakerFailures, cfg.BreakerCooldown, breakerOpts...)
	mws = append(mws, transport.CircuitBreaker(breaker))
	if cfg.tokens != nil {
		mws = append(mws, transport.Auth(cfg.tokens))
	}
	return transport.Chain(cfg.base, mws...)
}

// WithRoute returns a copy of ctx naming the route template of calls made
// with it, such as "/invoices/{id}", for logs and metrics. Without it the
// path is used with ID-like segments replaced by "{id}".
func WithRoute(ctx context.Context, route string) context.Context {
	return transport.WithRoute(ctx, route)
}

// canInvalidate reports whether a 401 can lead to a different token: only
// then is retrying it worthwhile.
func canInvalidate(ts auth.TokenSource) bool {
	_, ok := ts.(auth.Invalidator)
	return ok
}

// Idempotent returns a copy of ctx that opts a POST or PATCH made with it
// into an Idempotency-Key, which also makes it retryable. The key is the
// same on every attempt.
func Idempotent(ctx context.Context) context.Context { return transport.Idempotent(ctx) }

// newHTTPTransport returns a transport owned by one Client, never shared
// through http.DefaultTransport.
func newHTTPTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false // Protocols is authoritative
	t.Protocols = new(http.Protocols)
	t.Protocols.SetHTTP1(true)
	t.Protocols.SetHTTP2(true)
	t.HTTP2 = &http.HTTP2Config{
		// Health-check idle HTTP/2 connections so a dead peer is noticed
		// before a request hangs on it.
		SendPingTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
	return t
}

// checkRedirect follows redirects within the base URL's origin only. A
// redirect elsewhere is returned to the caller as a non-2xx [Error]: the
// middleware chain runs again for every hop, so following it would send
// credentials to another host.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("integration: stopped after %d redirects", maxRedirects)
	}
	if !sameOrigin(req.URL, c.cfg.Load().BaseURL) {
		return http.ErrUseLastResponse
	}
	return nil
}

// Do sends req with ctx. req must target the base URL's origin. On a 2xx
// response the caller must close resp.Body. Any other status becomes an
// [*Error] carrying the first 4 KiB of the body, which Do closes.
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("integration: nil request")
	}
	cfg := c.cfg.Load()
	if !sameOrigin(req.URL, cfg.BaseURL) {
		// Like http.Client.Do, close the body on every error path.
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("integration: %s: request to %s is outside base URL %s",
			cfg.Service, transport.RedactURL(req.URL), transport.RedactURL(cfg.BaseURL))
	}

	// Fix the request ID here, on the context, so errors can report it even
	// when no response arrives. The RequestID middleware sends it.
	id := req.Header.Get(transport.HeaderRequestID)
	if id == "" {
		var ok bool
		if id, ok = transport.RequestIDFromContext(ctx); !ok {
			id = uuid.New().String()
			ctx = transport.WithRequestID(ctx, id)
		}
	}

	req = req.WithContext(ctx)
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, transportError(ctx, cfg, req, id, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(cfg, req, resp, c.retry500)
	}
	return resp, nil
}

// Delete sends DELETE to path, resolved against the base URL.
func (c *Client) Delete(ctx context.Context, path string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// newRequest builds a request for path, resolved against the base URL.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u, err := resolve(c.cfg.Load().BaseURL, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("integration: build request: %w", unwrapURLError(err))
	}
	return req, nil
}

// resolve appends path (which may carry a query) to a clone of base. path
// must be relative, and may not contain "." or ".." segments, so that a
// path-escaped ID cannot climb out of the base path.
func resolve(base *url.URL, path string) (*url.URL, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("integration: path: %w", unwrapURLError(err))
	}
	if ref.Scheme != "" || ref.Host != "" || ref.User != nil || ref.Opaque != "" {
		return nil, fmt.Errorf("integration: path %q is not relative", transport.RedactURL(ref))
	}
	for seg := range strings.SplitSeq(ref.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("integration: path %q has a dot segment", ref.EscapedPath())
		}
	}

	u := base.Clone()
	if ref.Path != "" {
		u = u.JoinPath(ref.EscapedPath())
	}
	if ref.RawQuery != "" {
		q := base.Query() // a fresh map; base is untouched
		maps.Copy(q, ref.Query())
		u.RawQuery = q.Encode()
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// sameOrigin reports whether a and b share scheme, host and effective port.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// transportError wraps an error from the Doer. Caller cancellation is
// reported as context.Cause(ctx) and is not retryable.
func transportError(ctx context.Context, cfg *Config, req *http.Request, requestID string, err error) *Error {
	// *url.Error repeats the unredacted URL; Error.URL carries a safe copy.
	err = unwrapURLError(err)
	retryable := !errors.Is(err, ErrCircuitOpen)
	if ctx.Err() != nil {
		err = context.Cause(ctx)
		retryable = false
	}
	return &Error{
		Service:   cfg.Service,
		Method:    req.Method,
		URL:       transport.RedactURL(req.URL),
		Retryable: retryable,
		RequestID: requestID,
		Err:       err,
	}
}

// statusError turns a non-2xx response into an *Error, keeping at most
// maxErrorBody bytes of its body, and closes the body.
func statusError(cfg *Config, req *http.Request, resp *http.Response, retry500 bool) *Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody)) // diagnostics only
	resp.Body.Close()

	e := responseError(cfg, req, resp)
	e.Retryable = retryableStatus(resp.StatusCode, retry500)
	e.Body = body
	return e
}

// decodeError reports a 2xx response whose body could not be decoded. It
// is not retryable: the upstream answered, just not in the agreed shape.
func decodeError(cfg *Config, req *http.Request, resp *http.Response, err error) *Error {
	e := responseError(cfg, req, resp)
	e.Err = fmt.Errorf("decode response: %w", err)
	return e
}

// responseError fills the fields common to errors about a received
// response.
func responseError(cfg *Config, req *http.Request, resp *http.Response) *Error {
	// The request that reached the wire is a middleware clone; prefer its
	// headers, since that is where a generated request ID lives.
	sent := req
	if resp.Request != nil {
		sent = resp.Request
	}
	id := sent.Header.Get(transport.HeaderRequestID)
	if id == "" {
		id, _ = transport.RequestIDFromContext(req.Context())
	}
	return &Error{
		Service:    cfg.Service,
		Method:     req.Method,
		URL:        transport.RedactURL(sent.URL),
		StatusCode: resp.StatusCode,
		RequestID:  id,
	}
}

func unwrapURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// Compile-time check that the stdlib client satisfies Doer.
var _ Doer = (*http.Client)(nil)
