# Integration Layer — Next Steps

Open work after Phases 0–7. The status is in `docs/CLAUDE.md` and the plan in
`docs/integration/PLAN.md`. Items are roughly in priority order, and each says where it came from.

## 1. Phase 8: dynamic config

The only phase left. Deliverables are `WithDynamicConfig`, `ConfigSource`, and atomic config
wiring. The acceptance criteria are in `PLAN.md`:

- An update changes retry, breaker and limiter behaviour on the next request without rebuilding
  the chain.
- Invalid updates are rejected and logged, and the previous config stays active.
- Concurrent `Store` and request traffic pass under `-race`.

Design notes from building Phases 1–7:

- `Client.cfg` is already an `atomic.Pointer[Config]` holding an immutable clone. Only `Do` and
  `checkRedirect` read it today.
- The transports **copy settings when the chain is built**, in `buildChain`:
  - `transport.RetryConfig{MaxAttempts, AttemptTimeout}`;
  - `DefaultPolicy{Backoff}`, where `resilience.Backoff` holds base and max;
  - `resilience.NewTokenBucket(rate, burst)`;
  - `resilience.NewConsecutiveBreaker(failures, cooldown)`.

  Each needs to read through a getter, `func() *Config`, backed by the client's pointer. The
  natural shapes are a `Settings func() RetrySettings` field on `RetryConfig`, `SetRate` on
  `TokenBucket` (refill at the old rate up to "now", then switch), and threshold and cooldown read
  per call in `ConsecutiveBreaker`.
- **Fixed for the client's lifetime, and rejected if an update changes them:** `Service`,
  `BaseURL` and `Timeout`. `Timeout` is set on the `http.Client`; it could be moved into a
  per-call context deadline if it has to become dynamic.
- **Validation:** `ConfigSource.Watch(ctx, fn)` calls `fn` with each new config. Run
  `Config.Validate()` before `Store`. On failure, log through the configured logger (if any) and
  keep the old config.
- Make sure `NewHTTPClient` gets the same treatment, or document that it is static.

## 2. Retire the old `transport/` and `policy/` packages

They have had no callers outside each other since `fe510fa`, the Phase 7 migration of
`google.Client`. You decided to retire them "later". Deleting them is one commit. Two facts from
Phase 0 that matter only if they are kept instead:

- The old `transport.chain` calls each middleware factory on every request, so the old
  `RateLimit` never limits anything.
- The old `transport` tests were deleted in `4d1748d`; only `policy` has tests.

## 3. Migrate the remaining clients

The recipe and per-client notes are in `docs/integration/MIGRATING.md`.

- **`browser.Session`:** decide between one `integration.Client` per site behind a `Do(r)`
  adapter, or `NewHTTPClient` without credentials for a session that roams across hosts. Decide
  too how non-2xx pages should surface: the current `Session` returns them, while `Do` turns them
  into `*Error`.
- **`file.Download`:** needs a `ctx` parameter (a public API change; there are no in-repo
  callers), long timeouts, and no mid-stream retry unless resume via Range requests is added. Treat
  it as its own task.

## 4. Verify the Google migration against the live API

- Run a real Sheets/Drive smoke test with credentials. Every test so far uses fakes.
- The golden files in `google/testdata/` follow the documented Sheets v4 response shapes and are
  **not captured traffic**. Replace them with real recordings (secrets removed) when available.
- Decide whether one circuit breaker should span all Google hosts. A Sheets outage currently
  blocks Drive calls for 30s too. The alternative is one client per Google API.

## 5. Phase 0 findings still open

- **`google/google.go`, `TokenSource`:** the JWT, OAuth2 and ADC token sources make their token
  requests through `http.DefaultClient`, with no timeout or retry, because the context carries no
  `oauth2.HTTPClient`. Fix it by putting a dedicated client on the context:
  `context.WithValue(ctx, oauth2.HTTPClient, hc)`.
- **`google/google.go`, `waitForCode`:** it registers `/` on `http.DefaultServeMux`, so a second
  browser login in one process panics. Use a private `ServeMux`.
- **The token file:** it is written with `encoding/json` v1. Its `expires_in,omitempty` (`int64`)
  would be emitted as `0` under v2's `omitempty`. Keep it on v1, or pin it with a golden test
  before migrating.

## 6. Layer improvements (not required by the plan)

- **`Pages` envelopes:** an envelope or cursor variant of `Pages` for APIs that return
  `{"data": [...], "next": "..."}` rather than a bare array with a `Link` header.
- **`NewHTTPClient` hosts:** a host allowlist for credentials, so redirects or SDK calls to
  unexpected hosts don't receive the token.
- **OTel metrics:** an adapter in `otelx` implementing `transport.MetricsRecorder` (for example
  an `http.client.request.duration` histogram). This would add `go.opentelemetry.io/otel/metric`
  as a direct dependency, so ask first.
- **Attempt timeout coverage:** the per-attempt timeout covers the rate-limit wait and reading
  the body. If that bites (heavily throttled clients, slow downloads), start the attempt timer
  after the limiter, and stop it once headers arrive, using `context.WithCancelCause` plus
  `time.AfterFunc` in place of `WithTimeoutCause`. Update `docs/CLAUDE.md` if the rule changes.
- **Route templating:** `{id}` replacement is a heuristic, and free-form segments such as
  usernames pass through. Encourage `integration.WithRoute` at call sites, or add route-aware
  helpers.
- **Breaker metrics:** state changes are logged but not reported to `MetricsRecorder`.
