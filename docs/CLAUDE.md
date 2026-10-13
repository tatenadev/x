# CLAUDE.md — Service Integration Layer

Instructions for Claude Code when working on the outbound HTTP integration layer (`integration/`)
in this repo. Read this file first, then:

- `docs/integration/LAYOUT.md`: packages and interfaces.
- `docs/integration/PLAN.md`: the phased build plan with acceptance criteria.
- `docs/integration/INVENTORY.md`: the Phase 0 survey of existing HTTP clients.
- `docs/integration/MIGRATING.md`: the recipe for moving a client onto the layer.
- `docs/NEXT_STEPS.md`: open work, carried over for later.

## Context

This is an **existing codebase**, and the integration layer was built alongside code that already
made HTTP calls. Match existing conventions (naming, error style, test layout, lint config)
wherever they conflict with the defaults in this file, and note the conflict in your summary.

## Status

Phases 0–7 are complete on `main`. Phase 8 is the only phase left; it and the other open items
are in `docs/NEXT_STEPS.md`.

| Phase | Scope                                                                  | Status  | Commits                  |
|-------|------------------------------------------------------------------------|---------|--------------------------|
| 0     | Survey, repo facts, inventory (no Go upgrade needed)                   | ✅ Done | `1aefd9b`, `8e3c268`     |
| —     | Pre-Phase-1 baseline fix and `go fix`                                  | ✅ Done | `0046cb7`, `99cadb1`     |
| 1     | Core skeleton: `Client`, `Config`, `Error`, `Chain`, fakes             | ✅ Done | `5ef2f9c`                |
| 2     | Codecs (JSON v2, XML, form); `GetJSON`, `PostJSON`, `PutJSON`, `Pages` | ✅ Done | `d1b74a9`                |
| 3     | Auth: static, context, client credentials, `transport.Auth`            | ✅ Done | `0d77cd9`                |
| 4     | Retry, backoff, request IDs, idempotency keys                          | ✅ Done | `14e8d6e`                |
| 5     | Circuit breaker, rate limiter                                          | ✅ Done | `e1e0dfa`                |
| 6     | Logging, metrics, tracing; `otelx` OTel tracer adapter                 | ✅ Done | `b64358e`, `a7a13e4`     |
| 7     | Migrate `google.Client`; `NewHTTPClient`; `MIGRATING.md`               | ✅ Done | `b239a2a`, `fe510fa`     |
| 8     | Dynamic config (`WithDynamicConfig`, `ConfigSource`)                   | ⏳ Open | see `docs/NEXT_STEPS.md` |

Each phase passed `go build`, `go vet`, `go fix` (reviewed) and `go test -race -count=1 ./...`.

## Go version

This layer targets **Go 1.27 or later**. `go.mod` must declare `go 1.27` (or newer). Several
rules below depend on 1.27 features (generic methods, `encoding/json/v2`, `uuid`) and on
1.25–1.26 features (`testing/synctest`, `errors.AsType`, `new(expr)`, `go fix` modernizers).

`go test` runs the `stdversion` vet check by default since 1.27: it reports standard-library
symbols newer than the `go` directive. If it fires, the fix is the directive, not avoiding the API.

Do not set `GOEXPERIMENT=nojsonv2` or any removed `GODEBUG` opt-outs to make something compile.
Fix the code instead and flag the behavior change.

## Repo facts

Filled in during Phase 0 and kept current since. Update them when they change; do not guess.

- Module path: `github.com/tatenadev/x`
- `go` directive in `go.mod`: `go 1.27` (toolchain `go1.27.0`; no upgrade needed)
- Package root for this layer: `integration/` (public, `github.com/tatenadev/x/integration`).
  Subpackages: `transport`, `auth`, `codec`, `resilience`, `integrationtest`, `otelx`. It was
  built fresh. The old top-level `transport/` and `policy/` packages have **no callers since
  Phase 7** and are waiting to be retired (see `docs/NEXT_STEPS.md`). Do not import them from
  `integration/`. `integration/transport` shares a name with the old top-level `transport`, so
  alias one of them in any file that needs both.
- HTTP clients:
  - `google/client.go` (`Client`): **migrated in Phase 7**, built with
    `integration.NewHTTPClient`.
  - `browser/browser.go` (`Session`, injected `Doer`): not migrated.
  - `file/download.go` (`Download`, bare `http.Get`): not migrated.

  The oauth2 token sources in `google/google.go` still use `http.DefaultClient` implicitly.
  Details are in `docs/integration/INVENTORY.md`.
- Existing logger / metrics / tracing setup:
  - **The app:** logging is `charm.land/log/v2` (global logger, custom levels in root `log.go`),
    plus stdlib `log` in `file/`, `fsio/` and `paths/`.
  - **The layer:** `log/slog` via `WithLogger` (off unless configured), `MetricsRecorder` via
    `WithMetrics`, and `Tracer` via `WithTracer`. OTel is available through `otelx`.
  - **Correlation:** request IDs travel on ctx (`transport.WithRequestID`).
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
- Tracing: define a `Tracer` interface in core. The OpenTelemetry adapter lives in
  `integration/otelx`, approved in Phase 6, with `go.opentelemetry.io/otel` and `otel/trace` as
  direct requirements. It is the only layer package allowed to import OTel.
  `TestCoreImportsStdlibOnly` enforces this. Ask before adding any other OTel module.
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
func (c *Client) Pages[T any](ctx context.Context, path string, next PageFunc) iter.Seq2[T, error]
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
  - **The one exception:** third-party SDKs that require an `*http.Client` and call several hosts
    (the Google API libraries). Use `integration.NewHTTPClient`. It runs the same chain with no
    origin guard, so its credentials go to every host.
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

Outermost first. Do not change without updating this section. It is assembled in one place,
`buildChain` in `integration/client.go`, shared by `New` and `NewHTTPClient`.

```text
Tracing -> RequestID -> Metrics -> Logging -> [WithMiddleware] -> Retry -> RateLimit -> CircuitBreaker -> Auth -> http.Transport
```

- Tracing, Metrics and Logging are present only when a tracer, recorder or logger is configured.
  RateLimit is present only when a rate is configured. Auth is present only when a token source is
  configured. RequestID, Retry and CircuitBreaker are always present.
- Retry reports each attempt to hooks on the request context (`transport.ObserveAttempts`). That
  is how the outer middleware count attempts and emit per-attempt events.
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
  configurable (`WithRetryServerErrors`).
- `MaxAttempts` counts **total** attempts. The attempt timeout covers reading the body and any
  rate-limit wait. Retry gives up early when the next wait would outlast the context's deadline.
- Exponential backoff with full jitter, capped. Honour `Retry-After` on 429/503.
- Stop when the caller's context is done; return the wrapped `context.Cause(ctx)`.

### Auth

- Domain code never sets `Authorization`. Only `AuthTransport` does, from a `TokenSource`.
- The managed token source dedupes concurrent refreshes (one in flight; others wait) and
  refreshes before expiry with a skew (default 30s). Mutex plus shared result; no `singleflight`.
- On a 401, invalidate the cached token once and let Retry try again. The token is invalidated
  only if the rejected one is still current. The 401 is retried only when the source implements
  `auth.Invalidator`; otherwise a retry would resend the same token.
- Token-source failures are wrapped in `auth.ErrTokenUnavailable`, so they don't count against the
  upstream's circuit breaker.

### Logging and secrets

- `slog` with `*Context` methods so trace/request IDs on ctx are emitted.
- Never log `Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, or token fields. Redact by
  default.
- Body logging is off by default; when on, cap at 4 KiB and only for text content types. As built,
  it shows only JSON (sensitive members masked) and form bodies (sensitive fields masked). Other
  types are noted but not shown.
- The client does not log unless `WithLogger` is given. The app logs through `charm.land/log`, so
  defaulting to `slog.Default()` would add a line per call.

### Dynamic config (Phase 8, open)

- `atomic.Pointer[Config]`. Configs are immutable once stored; replace, never mutate.
- Validate before `Store`; reject invalid updates and keep the old config.
- Today the transports copy their settings when the chain is built. Phase 8 must switch them to
  a `func() *Config` getter. Design notes are in `docs/NEXT_STEPS.md`.

## Testing

- Table-driven tests; use `t.Context()` instead of `context.Background()`.
- **Time-dependent tests run inside `synctest.Test`** with `httptest.NewTestServer` (Go 1.27,
  in-memory network). Fake time advances instantly, so retry/backoff/breaker/token-expiry tests
  use real `time` calls with no injected clock and no real sleeping. Use `synctest.Sleep` to
  advance and settle. Do not add a `Clock` interface to production code.
- Start test servers with `integrationtest.Server(t, h)`. `srv.URL` is set only by the first
  `srv.Client()` or `Start` call. Reading `srv.URL` in the same argument list as `srv.Client()`
  depends on unspecified evaluation order.
- The `integration` tests' `newClient` helper makes a single attempt by default, so non-retry
  tests never wait on backoff. Retry behaviour has its own synctest tests.
- When a test guards an acceptance criterion, check that it fails when the behaviour is broken
  (a quick mutation), not only that it passes.
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
- Migrating existing callers is allowed since Phase 7. Follow `docs/integration/MIGRATING.md`
  and list every behaviour change in the commit or PR.
- If a rule here conflicts with existing repo code, follow the repo for that file and flag it.

## Pre-commit checklist

Check this for every commit. It applies to each change, so it is not a progress tracker.

- [ ] `go.mod` declares `go 1.27`+; no JSON/GODEBUG opt-outs added
- [ ] Requests built with `NewRequestWithContext`; ctx is first param
- [ ] Middleware clones before mutating and closes discarded bodies
- [ ] Errors checked with `errors.AsType`; `Error` uses pointer receivers
- [ ] No `http.DefaultClient` / `DefaultTransport`; no `atomic.Value`
- [ ] Time-based tests use `synctest`, not sleeps or injected clocks
- [ ] No new third-party imports without a stated reason
- [ ] `go vet`, `go fix` (reviewed), and `go test -race` pass
