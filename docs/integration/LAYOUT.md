# Integration Layer — Package Layout and Interfaces

Target package structure and core APIs for Claude Code to implement, written for Go 1.27+. Paths
are relative to the package root recorded in `CLAUDE.md` ("Repo facts"); adjust to the repo's
conventions.

## Packages

```text
integration/
  client.go          Client, New, Do, generic helpers (GetJSON, PostJSON, Pages)
  config.go          Config, defaults, Validate, atomic config holder
  options.go         Option and With* functions
  errors.go          Error, IsRetryable, sentinel errors
  doer.go            Doer interface
  transport/
    chain.go         Middleware type, Chain()
    requestid.go     X-Request-ID / Idempotency-Key via stdlib uuid
    retry.go         RetryTransport, RetryPolicy, default policy
    ratelimit.go     RateLimitTransport
    breaker.go       BreakerTransport
    auth.go          AuthTransport
    logging.go       LoggingTransport (slog, redaction)
    metrics.go       MetricsTransport
    tracing.go       TracingTransport (uses Tracer interface)
  auth/
    token.go         Token, TokenSource, Invalidator
    static.go        StaticToken, APIKey, BasicAuth
    contextauth.go   per-request token from ctx
    clientcreds.go   OAuth2 client-credentials manager (stdlib only)
  codec/
    codec.go         Codec interface, registry by content type
    json.go          encoding/json/v2
    xml.go
    form.go          struct -> url.Values via `form:"name"` tags
  resilience/
    backoff.go       exponential + full jitter (*rand.Rand), Retry-After parsing
    breaker.go       consecutive-failure breaker (closed/open/half-open)
    limiter.go       token bucket
  otelx/             optional OTel adapter for Tracer; only package importing OTel
  integrationtest/   FakeDoer, RecordingTransport, synctest helpers
```

Dependency direction: `transport` and `auth` depend on `resilience` and core interfaces; core
`integration` wires them together. Nothing in core imports `otelx`.

## Core interfaces

Interfaces stay non-generic: Go 1.27 interface methods cannot take type parameters, and generic
methods cannot satisfy interfaces.

```go
package integration

type Doer interface {
    Do(req *http.Request) (*http.Response, error)
}
```

```go
package transport

type Middleware func(http.RoundTripper) http.RoundTripper

// Chain applies mws so that mws[0] is outermost.
func Chain(base http.RoundTripper, mws ...Middleware) http.RoundTripper

type RetryPolicy interface {
    // ShouldRetry reports whether attempt (1-based) should be retried and
    // how long to wait. resp may be nil when err != nil.
    ShouldRetry(req *http.Request, resp *http.Response, err error, attempt int) (bool, time.Duration)
}

type MetricsRecorder interface {
    RecordRequest(ctx context.Context, info RequestInfo)
}

type RequestInfo struct {
    Service    string
    Method     string
    Route      string // templated path, never raw URL (cardinality)
    StatusCode int    // 0 on transport error
    Attempts   int
    Duration   time.Duration
    Err        error
}

type Tracer interface {
    Start(ctx context.Context, req *http.Request) (context.Context, func(resp *http.Response, err error))
    Inject(ctx context.Context, h http.Header)
}

// Optional: a Tracer that also implements this records each attempt (span event).
type AttemptTracer interface {
    TraceAttempt(ctx context.Context, a AttemptInfo)
}

// Retry reports every attempt to hooks registered on the request context;
// Tracing, Metrics and Logging use this to count attempts and emit events.
func ObserveAttempts(ctx context.Context, fn func(AttemptInfo)) context.Context
```

```go
package auth

type Token struct {
    Type   string // "Bearer", "Basic", or header name for API keys
    Value  string
    Expiry time.Time // zero = never expires
}

type TokenSource interface {
    Token(ctx context.Context) (Token, error)
}

// Invalidator is optionally implemented by caching sources so AuthTransport
// can force a refresh after a 401.
type Invalidator interface {
    Invalidate()
}
```

```go
package codec

type Codec interface {
    ContentType() string
    Encode(w io.Writer, v any) error // JSON: json.MarshalWrite
    Decode(r io.Reader, v any) error // JSON: json.UnmarshalRead
}
```

```go
package resilience

type Breaker interface {
    Allow() error // ErrCircuitOpen when open
    Record(success bool)
}

type Limiter interface {
    Wait(ctx context.Context) error
}
```

## Errors

```go
package integration

var (
    ErrCircuitOpen    = errors.New("integration: circuit open")
    ErrRateLimited    = errors.New("integration: rate limited")
    ErrInvalidConfig  = errors.New("integration: invalid config")
    ErrAttemptTimeout = errors.New("integration: attempt timed out") // used as context cause
)

type Error struct {
    Service    string
    Method     string
    URL        string // built from URL.Clone() with query secrets redacted
    StatusCode int    // 0 if no response
    Retryable  bool
    RequestID  string
    Body       []byte // first 4 KiB of error body, for diagnostics
    Err        error
}

// Pointer receivers only (see CLAUDE.md, Errors).
func (e *Error) Error() string
func (e *Error) Unwrap() error

func IsRetryable(err error) bool {
    if e, ok := errors.AsType[*Error](err); ok {
        return e.Retryable
    }
    return false
}
```

The resilience sentinels (`ErrCircuitOpen`, `ErrRateLimited`, `ErrAttemptTimeout`) are defined in
`resilience` and re-exported here, because `transport` needs them and cannot import `integration`.

Per-attempt timeouts use `context.WithTimeoutCause(ctx, d, ErrAttemptTimeout)`. Retry checks
`context.Cause` to tell an attempt timeout (retryable) from caller cancellation (terminal).

## Config and construction

```go
package integration

type Config struct {
    Service         string
    BaseURL         *url.URL
    Timeout         time.Duration // default 30s
    AttemptTimeout  time.Duration // default 10s
    MaxAttempts     int           // default 3
    BackoffBase     time.Duration // default 100ms
    BackoffMax      time.Duration // default 5s
    BreakerFailures int           // default 5
    BreakerCooldown time.Duration // default 30s
    RateLimit       float64       // req/s; 0 = unlimited
    RateBurst       int
}

func (c Config) Validate() error

type Option func(*config) error

func WithTimeout(d time.Duration) Option
func WithAttemptTimeout(d time.Duration) Option // default 10s, lowered to Timeout if shorter
func WithRetry(maxAttempts int, base, max time.Duration) Option
func WithRetryPolicy(p transport.RetryPolicy) Option
func WithRetryServerErrors() Option                   // also retry a plain 500
func WithIdempotencyKeys() Option                     // Idempotency-Key on every POST/PATCH
func WithRateLimit(rps float64, burst int) Option     // 1/rps must stay below AttemptTimeout
func WithBreaker(failures int, cooldown time.Duration) Option
func WithRand(r *rand.Rand) Option // math/rand/v2; deterministic jitter in tests
func WithTokenSource(ts auth.TokenSource) Option
func WithLogger(l *slog.Logger, opts ...transport.LoggingOption) Option // LogHeaders, LogBodies; no logger = no logging
func WithMetrics(m transport.MetricsRecorder) Option
func WithTracer(t transport.Tracer) Option
func WithBaseTransport(rt http.RoundTripper) Option // tests / custom TLS
func WithMiddleware(mws ...transport.Middleware) Option // appended inside Logging
func WithJSONOptions(opts ...json.Options) Option     // per-upstream v2 leniency
func WithDynamicConfig(src ConfigSource) Option        // Phase 8

type Client struct {
    cfg  atomic.Pointer[Config]
    doer Doer
    // ...
}

func New(service, baseURL string, opts ...Option) (*Client, error)

// Idempotent opts one POST/PATCH call into an Idempotency-Key (and retries).
func Idempotent(ctx context.Context) context.Context

// WithRoute names the route template ("/invoices/{id}") for logs and metrics.
// Without it, ID-like path segments are replaced by "{id}".
func WithRoute(ctx context.Context, route string) context.Context
```

`New` must: parse and validate `baseURL`, apply options, validate the final config, build a fresh
`*http.Transport` (set `Protocols` / `HTTP2Config` explicitly), assemble the chain in the order
defined in `CLAUDE.md`, and wrap it in an `*http.Client` with `Timeout` set.

## Generic helper methods

These rely on Go 1.27 generic methods. They are the primary API for domain services.

```go
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error)

func (c *Client) GetJSON[T any](ctx context.Context, path string) (T, error)
func (c *Client) PostJSON[Req, Resp any](ctx context.Context, path string, body Req) (Resp, error)
func (c *Client) PutJSON[Req, Resp any](ctx context.Context, path string, body Req) (Resp, error)
func (c *Client) Delete(ctx context.Context, path string) error

// Pages walks a paginated endpoint lazily. Stops on the first error
// (yielded as (zero, err)) or when the consumer breaks.
func (c *Client) Pages[T any](ctx context.Context, path string, next PageFunc) iter.Seq2[T, error]

// PageFunc extracts the next-page URL from a response; "" means done.
type PageFunc func(resp *http.Response) (string, error)
```

Request bodies set `GetBody` so they are retryable. Responses decode with `json.UnmarshalRead`
straight from `resp.Body`, which is then closed.

## Dynamic config (Phase 8)

```go
type ConfigSource interface {
    // Watch calls fn with each new config until ctx is done.
    Watch(ctx context.Context, fn func(Config)) error
}
```

Transports read thresholds through a `func() *Config` getter backed by the client's
`atomic.Pointer`, so updates apply on the next request without rebuilding the chain.

## Example domain service

```go
type BillingClient struct {
    c *integration.Client
}

func NewBillingClient(baseURL string, ts auth.TokenSource, opts ...integration.Option) (*BillingClient, error) {
    c, err := integration.New("billing", baseURL, append(opts, integration.WithTokenSource(ts))...)
    if err != nil {
        return nil, fmt.Errorf("billing: %w", err)
    }
    return &BillingClient{c: c}, nil
}

func (b *BillingClient) GetInvoice(ctx context.Context, id string) (Invoice, error) {
    inv, err := b.c.GetJSON[Invoice](ctx, "/invoices/"+url.PathEscape(id))
    if err != nil {
        return Invoice{}, fmt.Errorf("get invoice %s: %w", id, err)
    }
    return inv, nil
}

func (b *BillingClient) Invoices(ctx context.Context) iter.Seq2[Invoice, error] {
    return b.c.Pages[Invoice](ctx, "/invoices", integration.LinkHeaderNext)
}
```

## Example time-based test

```go
func TestRetryBackoff(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        var calls atomic.Int32
        srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if calls.Add(1) < 3 {
                w.WriteHeader(http.StatusServiceUnavailable)
                return
            }
            w.WriteHeader(http.StatusOK)
        }))

        c, err := integration.New("test", srv.URL,
            integration.WithBaseTransport(srv.Client().Transport),
            integration.WithRand(rand.New(rand.NewPCG(1, 2))),
        )
        if err != nil {
            t.Fatal(err)
        }

        start := time.Now()
        if err := c.Delete(t.Context(), "/x"); err != nil {
            t.Fatal(err)
        }
        // Fake time: elapsed equals summed backoff, test runs instantly.
        if elapsed := time.Since(start); elapsed > 5*time.Second {
            t.Fatalf("backoff too long: %v", elapsed)
        }
    })
}
```

`integrationtest.Server` wraps `httptest.NewTestServer(t testing.TB, handler http.Handler)`
(verified against go1.27.0). It calls `srv.Client()` before returning, because `srv.URL` is only
set by the first `Client`/`Start` call; reading `srv.URL` and calling `srv.Client()` in the same
argument list relies on unspecified evaluation order.
