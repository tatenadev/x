# CLAUDE.md — Service Integration Layer

Instructions for Claude Code when working on the outbound HTTP integration layer in this repo.
Read this file first, then `docs/integration/LAYOUT.md` (packages and interfaces) and
`docs/integration/PLAN.md` (phased build plan with acceptance criteria).

## Context

This is an **existing codebase**. The integration layer is being introduced alongside code that
already makes HTTP calls. Before writing anything, complete Phase 0 in `PLAN.md` and fill in the
"Repo facts" section below. Match existing conventions (naming, error style, test layout, lint
config) wherever they conflict with the defaults in this file, and note the conflict in your
summary.

## Go version

This layer targets **Go 1.27 or later**. `go.mod` must declare `go 1.27` (or newer). Several
rules below depend on 1.27 features (generic methods, `encoding/json/v2`, `uuid`) and on
1.25–1.26 features (`testing/synctest`, `errors.AsType`, `new(expr)`, `go fix` modernizers).

`go test` runs the `stdversion` vet check by default since 1.27: it reports standard-library
symbols newer than the `go` directive. If it fires, the fix is the directive, not avoiding the API.

Do not set `GOEXPERIMENT=nojsonv2` or any removed `GODEBUG` opt-outs to make something compile.
Fix the code instead and flag the behavior change.

## Repo facts

Fill these in during Phase 0. Do not guess.

- Module path: `github.com/tatenadev/x`
- `go` directive in `go.mod`: `go 1.27` (toolchain `go1.27.0`; no upgrade needed)
- Package root for this layer: `integration/` (public, `github.com/tatenadev/x/integration`).
  It is built fresh. The existing top-level `transport/` and `policy/` packages stay as they
  are and are retired later, after their callers migrate. Do not import them from `integration/`.
  `integration/transport` shares a name with the old top-level `transport`, so alias one of them
  in any file that needs both.
- Existing HTTP clients found: `google/client.go` (`Client`, oauth2 + `transport.Retry`),
  `browser/browser.go` (`Session`, injected `Doer`), `file/download.go` (`Download`, bare
  `http.Get`). Shared resilience code: `transport/`, `policy/`. Implicit `http.DefaultClient`
  in the oauth2 token sources in `google/google.go`. Details in `docs/integration/INVENTORY.md`.
- Existing logger / metrics / tracing setup: logging is `charm.land/log/v2` (global logger,
  custom levels in root `log.go`), plus stdlib `log` in `file/`, `fsio/` and `paths/`. No
  `slog`, no metrics, no tracing, and no correlation IDs on ctx.
- Lint/static analysis in use: `go vet` only. No staticcheck or golangci-lint config, no CI
  config in the repo.

## Commands

Run these before declaring any phase done.

```bash
go build ./...
go vet ./...
go fix ./...
go test -race -count=1 ./...
```

`go fix` applies modernizers (including `atomictypes`). Review its diff and commit it separately
from feature work so reviewers can tell mechanical changes from real ones. Go 1.27 `gofmt` may
make small alignment-only whitespace changes to old files; commit those separately too.

If the repo has `staticcheck` or `golangci-lint` configured, run it as well.

## Dependency policy

Use the standard library. Each third-party import needs a stated reason in the PR summary.

- Logging: `log/slog`. Do not add zap/zerolog/logrus.
- JSON: `encoding/json/v2` (and `encoding/json/jsontext` if streaming tokens). No third-party
  JSON libraries.
- IDs: the standard `uuid` package for request IDs and idempotency keys. Do not add
  `github.com/google/uuid`. Check `go doc uuid` for the exact API rather than guessing.
- Metrics: define a `MetricsRecorder` interface; no Prometheus import in core packages.
- Tracing: define a `Tracer` interface in core. An OpenTelemetry adapter may live in its own
  subpackage (`.../otelx`) so core stays dependency-free. Ask before adding OTel to `go.mod`.
- Backoff, circuit breaker, rate limiter, token refresh dedup: implement in-house.
  `golang.org/x/time/rate` is the one acceptable exception if a token bucket proves
  non-trivial — ask first.

## Go 1.27 idioms to use

Each rule names the feature and why it applies here.

### Generic methods for typed helpers

Go 1.27 allows methods to declare their own type parameters. Put typed request helpers on
`*Client` instead of package-level functions, so call sites read `c.GetJSON[Invoice](ctx, p)`.

```go
func (c *Client) GetJSON[T any](ctx context.Context, path string) (T, error)
func (c *Client) PostJSON[Req, Resp any](ctx context.Context, path string, body Req) (Resp, error)
func (c *Client) Pages[T any](ctx context.Context, path string) iter.Seq2[T, error]
```

Constraints to respect: interface methods cannot have type parameters, and a generic method
never satisfies an interface. So `Doer`, `TokenSource`, `Codec` and other interfaces stay
non-generic, and mocking happens at the `Doer`/transport level, not by faking `*Client`.

### Errors

- Branch with `errors.AsType[*integration.Error](err)` (Go 1.26), not `errors.As` with a
  pre-declared variable. It is type-safe and allocation-free.
- Give `Error` pointer-receiver methods only. Go 1.27 vet flags `%w` of a `*E` when `E` itself
  implements `error`; pointer-only receivers avoid that trap and keep `errors.Is` correct.
- Distinguish client timeout from caller cancellation with `context.WithTimeoutCause` and
  `context.Cause`. A per-attempt timeout is retryable; caller cancellation is not.

### JSON

- The JSON codec uses `encoding/json/v2` with `MarshalWrite` / `UnmarshalRead` directly on the
  request/response body. No intermediate `[]byte` for responses.
- Keep v2's strict defaults (reject duplicate object names and invalid UTF-8). If a specific
  upstream needs lenient behavior, enable the relevant option for that client only and comment
  why.
- v2 differs from v1 in name matching, nil slice/map encoding and `omitempty` semantics. When
  migrating an existing client, check the `encoding/json` "Migrating to v2" docs and add a test
  pinning the upstream's actual payloads.
- Use `omitzero` for optional fields. The `inline` tag option is now `embed`.
- For optional pointer fields in DTOs and config, use `new(expr)` (Go 1.26), e.g.
  `Limit: new(50)`, instead of temp variables or `ptr()` helpers.

### HTTP and URLs

- Since 1.27, closing an HTTP/1 `Response.Body` drains unread content up to a conservative limit.
  Always `Close()`; do not hand-roll unbounded `io.Copy(io.Discard, ...)`. When you need the
  body (error diagnostics), read through `io.LimitReader` (4 KiB), then close.
- Use `URL.Clone()` and `Values.Clone()` (Go 1.27) when deriving request URLs from `BaseURL` or
  building redacted copies for logs. Struct-copying a `url.URL` shares the `User` pointer.
- Configure the transport with `http.Transport.Protocols` and `HTTP2Config` rather than
  `golang.org/x/net/http2`.

### Concurrency and time

- Use `atomic.Pointer[T]`, `atomic.Int64` etc., never `atomic.Value` or raw `atomic.AddInt64`
  (`go fix` `atomictypes` enforces this).
- Timer channels are always synchronous. Write waits as `t := time.NewTimer(d); defer t.Stop()`
  plus a `select` on `ctx.Done()`. No drain-after-Stop dance.
- Jitter uses `math/rand/v2`. Take a `*rand.Rand` via option and call its generic `N` method
  (`r.N(maxDelay)` works directly on `time.Duration`).
- Use `sync.WaitGroup.Go` for spawning goroutines that must be awaited.

## Architecture rules

### Interfaces and construction

- The transport stack depends on `Doer`; services depend on `*integration.Client` (built with a
  fake base transport in tests) or a small interface the service defines itself. Never on
  `*http.Client`.
- Accept interfaces, return structs. Constructors validate and return `(T, error)`, so a
  misconfigured client fails at startup.
- Functional options write to an unexported `config`: `type Option func(*config) error`.
- Never use `http.DefaultClient` or `http.DefaultTransport` directly. Build a fresh
  `*http.Transport` (clone `DefaultTransport` if useful) and an `*http.Client` with `Timeout`.

### Context

- Every network-bound function takes `ctx context.Context` first.
- Build requests with `http.NewRequestWithContext`. Never `http.NewRequest`.
- Never store a context in a struct.

### RoundTripper middleware

- Middleware is `type Middleware func(http.RoundTripper) http.RoundTripper`.
- **Clone the request before mutating it** (`req.Clone(req.Context())`). The `RoundTripper`
  contract forbids modifying the caller's request; violating it causes races under retry.
- A middleware that does not return a response to its caller must `Close()` its body.
- Encoding (JSON/XML/form) lives in `codec`, never in a transport.

### Middleware order

Outermost first. Do not change without updating this section.

```text
Tracing -> RequestID -> Metrics -> Logging -> Retry -> RateLimit -> CircuitBreaker -> Auth -> http.Transport
```

- Tracing, RequestID, Metrics and Logging sit outside Retry: one span, one ID, one latency sample
  and one log line per logical call. Attempts appear as span events and debug logs.
- RequestID sets `X-Request-ID` (stdlib `uuid`) if absent, and sets `Idempotency-Key` for
  opted-in POST/PATCH so every retry carries the same key.
- **Auth sits inside Retry** so every attempt re-fetches the token and a refresh can happen
  mid-retry.
- RateLimit and CircuitBreaker sit inside Retry because each attempt hits the upstream. An open
  breaker returns `ErrCircuitOpen`, which is terminal.

### Retries

- Retry only idempotent methods (GET, HEAD, OPTIONS, PUT, DELETE) by default. POST/PATCH only
  with an idempotency key.
- A request with a body is retryable only if `req.GetBody != nil`; rewind via `GetBody`.
- Retry on network errors, per-attempt timeouts, 429, 502, 503, 504. Not other 4xx; 500 is
  configurable.
- Exponential backoff with full jitter, capped. Honour `Retry-After` on 429/503.
- Stop when the caller's context is done; return the wrapped `context.Cause(ctx)`.

### Auth

- Domain code never sets `Authorization`. Only `AuthTransport` does, from a `TokenSource`.
- The managed token source dedupes concurrent refreshes (one in flight; others wait) and
  refreshes before expiry with a skew (default 30s). Mutex plus shared result; no `singleflight`.
- On a 401, invalidate the cached token once and let Retry try again.

### Logging and secrets

- `slog` with `*Context` methods so trace/request IDs on ctx are emitted.
- Never log `Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, or token fields. Redact by
  default.
- Body logging is off by default; when on, cap at 4 KiB and only for text content types.

### Dynamic config

- `atomic.Pointer[Config]`. Configs are immutable once stored; replace, never mutate.
- Validate before `Store`; reject invalid updates and keep the old config.

## Testing

- Table-driven tests; use `t.Context()` instead of `context.Background()`.
- **Time-dependent tests run inside `synctest.Test`** with `httptest.NewTestServer` (Go 1.27,
  in-memory network). Fake time advances instantly, so retry/backoff/breaker/token-expiry tests
  use real `time` calls with no injected clock and no real sleeping. Use `synctest.Sleep` to
  advance and settle. Do not add a `Clock` interface to production code.
- A `synctest` bubble panics if goroutines are left durably blocked, which doubles as a
  goroutine-leak check. For hangs outside tests, use the `goroutineleak` pprof profile.
- Every middleware gets a test proving it does not mutate the input request and closes bodies it
  discards.
- Concurrency-sensitive code (token manager, breaker, atomic config) gets a `-race` test with
  many goroutines.
- Make jitter deterministic in tests by passing a seeded `*rand.Rand`.
- Benchmarks use `for b.Loop()`.

## Working style

- One phase of `PLAN.md` at a time. End each phase with: what changed, commands run and results,
  anything deferred.
- Do not migrate existing callers until Phase 7. Until then, new code must not change existing
  behavior.
- If a rule here conflicts with existing repo code, follow the repo for that file and flag it.

## Pre-commit checklist

- [ ] `go.mod` declares `go 1.27`+; no JSON/GODEBUG opt-outs added
- [ ] Requests built with `NewRequestWithContext`; ctx is first param
- [ ] Middleware clones before mutating and closes discarded bodies
- [ ] Errors checked with `errors.AsType`; `Error` uses pointer receivers
- [ ] No `http.DefaultClient` / `DefaultTransport`; no `atomic.Value`
- [ ] Time-based tests use `synctest`, not sleeps or injected clocks
- [ ] No new third-party imports without a stated reason
- [ ] `go vet`, `go fix` (reviewed), and `go test -race` pass
