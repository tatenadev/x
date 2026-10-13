# Integration Layer — Build Plan

Phased plan for Claude Code, targeting Go 1.27+. Do one phase per session or PR. A phase is done
only when every acceptance criterion holds and the commands in `CLAUDE.md` pass.

## Phase 0 — Survey and toolchain upgrade

Mostly read-only. The only code change allowed is the Go version bump below.

Deliverables:

- Fill in "Repo facts" in `CLAUDE.md`.
- List every place that constructs `http.Client`, uses `http.DefaultClient`, `http.Get/Post`,
  or `http.NewRequest` without context. Write it to `docs/integration/INVENTORY.md` with file,
  function, and a one-line note on auth/retry behavior.
- In the inventory, also note JSON behavior each existing client relies on (case-insensitive
  field matching, `null` vs `[]` for empty slices, `omitempty`, duplicate keys in upstream
  payloads). These matter when moving to `encoding/json/v2` in Phase 7.
- Note existing logging, metrics, tracing, and how ctx carries correlation IDs.

If `go.mod` declares less than `go 1.27`, do the upgrade as its own commit before any layer work:

```bash
go get go@1.27
go fix ./...
gofmt -l .
go test -race -count=1 ./...
```

Acceptance:

- `go.mod` declares `go 1.27` or newer; toolchain upgrade and `go fix` changes are isolated
  commits with green tests.
- No `GOEXPERIMENT=nojsonv2` or removed `GODEBUG` settings were introduced to get green.
- Inventory covers all matches from `grep -rn "http\.\(Client\|DefaultClient\|Get\|Post\|NewRequest\)"`.

## Phase 1 — Core skeleton

Deliverables: `doer.go`, `errors.go`, `config.go`, `options.go`, `client.go`,
`transport/chain.go`, `integrationtest/` fakes.

Acceptance:

- `New` returns an error for empty service, unparsable or non-http(s) base URL, non-positive
  timeout, `MaxAttempts < 1`.
- `Chain` applies middleware with `mws[0]` outermost (test with recording middleware).
- `Client.Do` and `Client.Delete` exist; request URLs are derived with `BaseURL.Clone()`.
- Non-2xx responses become `*Error` with status, redacted URL, and body read via
  `io.LimitReader` (4 KiB).
- `IsRetryable` uses `errors.AsType`; tests assert with `errors.AsType`, not string matching.
- `Error` has pointer receivers only; `go vet` is clean.

## Phase 2 — Codecs and generic helpers

Deliverables: `codec/` JSON (v2), XML, form; `GetJSON`, `PostJSON`, `PutJSON`, `Pages` generic
methods on `*Client`.

Acceptance:

- JSON codec uses `json.MarshalWrite` / `json.UnmarshalRead`; no `io.ReadAll` of response bodies.
- v2 strict defaults hold: a duplicate-key payload fails to decode (test it).
  `WithJSONOptions` can relax this per client.
- Helpers set `Content-Type` and `Accept` from the codec and set `GetBody` on requests.
- Form codec handles `form:"name,omitempty"` tags, strings, ints, bools, slices.
- `Pages` stops when the consumer breaks out of the loop (no extra request made) and yields
  errors as `(zero, err)` then stops.
- A small DTO test uses `new(expr)` for an optional pointer field and `omitzero`.

## Phase 3 — Auth

Deliverables: `auth/` static, context, client-credentials; `transport/auth.go`.

Acceptance:

- `AuthTransport` clones the request and does not overwrite an existing `Authorization` unless
  configured to.
- Client-credentials manager: 100 concurrent callers on an expired token trigger exactly one
  refresh (counting fake server, `-race`, goroutines spawned with `sync.WaitGroup.Go`).
- Refresh-before-expiry is tested inside `synctest.Test` by sleeping fake time past
  `expiry - skew`. No `Clock` interface in production code.
- On 401, `Invalidate()` is called once and the next attempt gets a fresh token.

## Phase 4 — Retry and backoff

Deliverables: `resilience/backoff.go`, `transport/retry.go`, `transport/requestid.go`.

Acceptance:

- Retries only the methods and statuses listed in `CLAUDE.md`; table test covers each.
- Body is replayed via `GetBody`; a request with a body and no `GetBody` is not retried.
- Per-attempt timeout uses `context.WithTimeoutCause(..., ErrAttemptTimeout)` and is retried;
  caller cancellation is not, and the returned error wraps `context.Cause(ctx)`.
- `Retry-After` (seconds and HTTP-date) is honoured, capped at `BackoffMax`.
- Jitter comes from the injected `*rand.Rand` via its generic `N` method; a seeded source gives
  deterministic delays in tests.
- All timing tests run in `synctest.Test` with `httptest.NewTestServer`; the suite has no real
  sleeps.
- `Idempotency-Key` is identical across all attempts of an opted-in POST.
- Discarded responses are closed (test with a body that records `Close`).

## Phase 5 — Circuit breaker and rate limiter

Deliverables: `resilience/breaker.go`, `resilience/limiter.go`, matching transports.

Acceptance:

- Breaker opens after N consecutive failures, rejects with `ErrCircuitOpen` during cooldown,
  allows one probe in half-open, closes on probe success. Cooldown tested with `synctest`.
- Only transport errors and 5xx count as failures; 4xx do not.
- `ErrCircuitOpen` is non-retryable; Retry stops on it.
- Limiter `Wait` respects ctx cancellation; burst behaves as configured.
- State uses `atomic` typed values or a mutex; both safe under `-race`.

## Phase 6 — Observability

Deliverables: `transport/logging.go`, `metrics.go`, `tracing.go`; optional `otelx/`.

Acceptance:

- One log line per logical call at Info (Warn on error) with service, method, route, status,
  attempts, duration, request ID; per-attempt detail at Debug.
- Redaction test: none of the sensitive headers in `CLAUDE.md` appear in log output, and URLs
  in logs come from a redacted `URL.Clone()`.
- Metrics use templated route, never raw URL.
- `Tracer.Inject` is called on the outgoing request; finish func receives final resp/err.
- Core packages have no third-party imports (`go list -deps` check). If `otelx` exists, it is
  the only package importing OTel.

## Phase 7 — Migrate one existing integration

Pick the simplest client from `INVENTORY.md`.

Acceptance:

- Its public API is unchanged, or changes are listed and callers updated.
- JSON behavior differences found in Phase 0 are either preserved via `WithJSONOptions` or
  accepted explicitly; a test pins real upstream payloads (golden files).
- Existing tests pass; new tests use `integrationtest.FakeDoer` or `httptest.NewTestServer`.
- Behavior differences (new retries, timeouts, JSON strictness) are listed in the PR summary.
- Write a migration recipe in `docs/integration/MIGRATING.md` for the remaining clients.

## Phase 8 — Dynamic config

Deliverables: `WithDynamicConfig`, `ConfigSource`, atomic config wiring.

Acceptance:

- Updating config changes retry/breaker/limiter behavior on the next request without rebuilding
  the chain.
- Invalid updates are rejected and logged; previous config stays active.
- Concurrent `Store` and request traffic pass under `-race`.

## Deferred

gRPC/GraphQL/SOAP wrappers. When needed, reuse `Config`, `RetryPolicy`, `Breaker`, `Limiter`
and `TokenSource` behind a protocol-specific invoker rather than forcing them through `Doer`.
