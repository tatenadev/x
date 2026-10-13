# Integration Layer — Phase 0 Inventory

Surveyed at `4d1748d` (2026-10-13) with `go1.27.0 linux/amd64`.

## Grep coverage

`grep -rn "http\.\(Client\|DefaultClient\|Get\|Post\|NewRequest\)" --include=*.go .` returns 6 lines.
Each one is covered below:

| Match                       | Section                            |
|-----------------------------|------------------------------------|
| `google/client.go:36,38`    | [google.Client](#googleclient)     |
| `browser/browser.go:92`     | [browser.Session](#browsersession) |
| `file/download.go:24`       | [file.Download](#filedownload)     |
| `ui/web/web_test.go:28,52,72` | [Test-only](#test-only-callers)  |

A wider search (`DefaultTransport`, `oauth2.Transport`, `RoundTrip`, `option.WithHTTPClient`,
`NewService`) found the implicit default-client uses listed under
[Hidden default clients](#hidden-default-clients).

## Outbound HTTP clients

### `google.Client`

> **Migrated in Phase 7.** It is now built with `integration.NewHTTPClient` and an `oauth2` token
> source adapter. The rest of this section describes the client as Phase 0 found it. The old
> top-level `transport` and `policy` packages no longer have callers. See `MIGRATING.md`.

- **File / func:** `google/client.go` `Client(ctx, xdg, scope...) (*http.Client, error)`
- **Builds:** `transport.New(WithBaseTransport(oauth2.Transport{Source: ts}.RoundTrip),
  WithMiddleware(transport.Retry(...)))` wrapped in `&http.Client{Transport: ...}`. Until
  `0046cb7` it called `.Client()`, which `4d1748d` removed.
- **Auth:** `oauth2.Transport` with the token source from `google.TokenSource` (JWT service
  account, then a persisted OAuth2 user token with a browser flow, then ADC). `oauth2.Transport.Base`
  is nil, so it falls through to **`http.DefaultTransport`**. The auth step sits inside Retry, so
  every attempt re-reads the token. That matches the target order.
- **Retry:** `transport.Retry(transport.Retrier{Backoff: policy.DefaultBackoff})`, which is
  200 ms base, ×2, 10 s cap, **3 retries after the first try (4 attempts)**, and equal jitter.
  See [existing transport](#existing-transport--policy-packages) for how it classifies errors.
- **Timeout:** none. The repo used to rely on `Transport.Client()` and set none on the client.
- **Consumers:** no in-repo callers. `gdrive`/`gsheets` take a `*drive.Service` or
  `*sheets.Service`, and the caller builds it outside the repo (presumably via
  `option.WithHTTPClient`).
- **JSON:** request and response bodies go through the generated `google.golang.org/api`
  clients, which use their own `encoding/json` (v1 API) with `gensupport`. That is **not under our
  control**, so moving to json/v2 does not apply to the wire format. The repo's own JSON next to
  it:
  - `google/google.go` `tokenFromFile` / `saveToken`: `oauth2.Token` round-trips through a local
    file with `encoding/json` v1 `Decoder`/`Encoder`. The tags are `access_token`,
    `token_type`, `refresh_token`, `expiry,omitempty` (`time.Time`) and `expires_in,omitempty`.
    The repo writes the file itself, so field names are exact-case and there are no duplicate
    keys. Two things to watch under v2 `omitempty`: `time.Time` is never omitted in v1 or v2,
    and the zero-valued `expires_in` int64 is omitted in v1 but **not omitted in v2**
    (v2 `omitempty` omits only empty JSON values; numbers need `omitzero`). The file is external
    to us. Keep it on `encoding/json` v1 or pin it with a golden test before touching it.
  - `gsheets/types.go` DTOs (`Sheet`, `SheetInfo`) use `omitempty` on strings and on `int64 GID`.
    Under v2, `GID: 0` would be **emitted** instead of omitted. These are not HTTP payloads
    today.

### `browser.Session`

- **File / func:** `browser/browser.go` `(*Session).Get` → `do`
- **Builds:** `http.NewRequestWithContext(ctx, GET, rawURL, nil)` and sends it through a
  caller-supplied `browser.Doer` (`Do(*http.Request)`), so it never builds a client itself. The
  doc comment refers to a `*client.Client`, which no longer exists.
- **Auth:** none. It manages cookies by hand from a `cookiejar` (`req.AddCookie` + `SetCookies`),
  sets `User-Agent: ctld-browser/1.0`, and **mutates the caller-built request in place**. That
  is fine for a request it built itself, but note it.
- **Retry:** whatever the injected `Doer` does. None by default.
- **Body:** `io.ReadAll(resp.Body)` with no limit (an HTML scraper). It does not treat non-2xx
  as an error: it returns `Page{StatusCode}`.
- **JSON:** none (HTML only).
- **Fit:** a good first Phase 7 candidate. It already depends on a `Doer` interface identical to
  the planned `integration.Doer`, and it has `httptest` tests.

### `file.Download`

- **File / func:** `file/download.go` `Download(url, filepath string) error`
- **Builds:** **`http.Get(url)`**, which means `http.DefaultClient`, **no context and no timeout**.
- **Auth:** none. **Retry:** none.
- **Body:** streams `io.Copy` to a file, so the size is unbounded by design. Any non-200 →
  `fmt.Errorf("bad status: %s")`, and the response body is closed via `closeCloser`.
- **Logging:** stdlib `log` (the `ProgressWriter`/`ProgressReader` helpers in the same file).
- **JSON:** none.
- **Callers:** none in the repo.
- **Fit:** simplest code, but migrating it changes its signature (it needs a `ctx` parameter).
  Phase 7 would have to list this as a public API change.

## Hidden default clients

These do not match the grep but still reach `http.DefaultClient` or `http.DefaultTransport`:

| Where                                    | Mechanism                                                                                      |
|------------------------------------------|------------------------------------------------------------------------------------------------|
| `transport/transport.go:26` `New`        | Default base is `http.DefaultTransport.RoundTrip`.                                             |
| `google/client.go:48`                    | `oauth2.Transport{Base: nil}` → `http.DefaultTransport`.                                       |
| `google/google.go` `TokenSource`         | `jwt.Config.TokenSource`, `oauth2.Config.TokenSource`/`Exchange` and `google.DefaultTokenSource` make token-endpoint calls through `oauth2/internal.ContextClient(ctx)`. The ctx carries no `oauth2.HTTPClient` value, so they fall back to **`http.DefaultClient`** (no timeout, no retry). |

## Existing transport / policy packages

The repo already has a home-grown HTTP resilience layer. It overlaps with, and in places
conflicts with, `LAYOUT.md`:

| Concern            | Existing (`transport/`, `policy/`)                                                    | `docs/CLAUDE.md` / `LAYOUT.md` target                                    |
|--------------------|----------------------------------------------------------------------------------------|---------------------------------------------------------------------|
| Package path       | top-level `transport`, `policy`                                                        | `<root>/integration/transport`, `<root>/integration/resilience`     |
| Middleware type    | `func(transport.RoundTrip) transport.RoundTrip` (`RoundTrip` is a func type)           | `func(http.RoundTripper) http.RoundTripper`                         |
| Chain order        | `mw[0]` outermost                                                                      | same                                                                |
| Default base       | `http.DefaultTransport`                                                                | fresh `*http.Transport` with `Protocols`/`HTTP2Config`              |
| Attempts           | `Backoff.MaxAttempts` = retries **after** the first try                                | `MaxAttempts` = total attempts, default 3                           |
| Jitter             | equal jitter `[d/2, d]`, global `math/rand/v2` `Float64`, not injectable               | full jitter, injected `*rand.Rand`, `r.N(d)`                        |
| Retry statuses     | any network error; **all 5xx (incl. 500)** + 429 for idempotent methods                | network/attempt-timeout, 429, 502, 503, 504; 500 configurable       |
| Non-idempotent     | POST/PATCH **retried on 429/503** without an idempotency key                            | POST/PATCH only with `Idempotency-Key`                              |
| Idempotent set     | GET HEAD PUT DELETE OPTIONS **TRACE**                                                  | GET HEAD OPTIONS PUT DELETE                                         |
| Body replay        | `GetBody` if set, else **buffers with unbounded `io.ReadAll`**                         | retry only if `GetBody != nil`                                      |
| Retry-After        | seconds + HTTP-date, capped at `Backoff.Max`                                           | same                                                                |
| Discard            | `io.CopyN(io.Discard, body, 64 KiB)` + `Close`                                         | just `Close()` (1.27 drains)                                        |
| Attempt timeout    | none                                                                                   | `context.WithTimeoutCause(..., ErrAttemptTimeout)`                  |
| Ctx error          | returns `ctx.Err()`                                                                    | wrapped `context.Cause(ctx)`                                        |
| Errors             | `fmt.Errorf("transport: %w")`, no typed error                                          | `*integration.Error`, `errors.AsType`                               |
| Rate limiter       | `policy.Limiter` token bucket (mutex, lazy refill, synctest-friendly)                  | `resilience/limiter.go` token bucket                                |
| Generic method     | `policy.Policy.Do[T]`, already a Go 1.27 generic method                                | —                                                                   |
| Tests              | `policy` has synctest tests; `transport` tests were **deleted** in `4d1748d`           | every middleware tested                                             |

**Bug found (not fixed, Phase 0 is read-only):** `transport.chain` calls `m(next)` inside the
per-request closure, so middleware factories run **on every request**. `transport.RateLimit` builds
its `policy.Limiter` inside the factory, which gives each request a fresh, full bucket. **Rate
limiting has no effect.** `Retry` is stateless, so it is unaffected.

## JSON behaviour summary (for Phase 7)

No in-repo HTTP client decodes JSON from an upstream itself. The candidates for a json/v2
migration are the local-file and DTO uses next to the clients:

| Site                         | Relies on                                                                                       |
|------------------------------|-------------------------------------------------------------------------------------------------|
| `google/google.go` token file | v1 `omitempty` on `int64 expires_in` (v2 would emit `0`); exact-case keys; no duplicates        |
| `gsheets/types.go` DTOs      | v1 `omitempty` on `int64 GID` (v2 would emit `0`); `omitzero` needed to keep the shape            |
| `parse/json/json.go:40`      | `DisallowUnknownFields` (v2 option: `json.RejectUnknownMembers(true)`)                           |
| Google API wire payloads     | handled by `google.golang.org/api`; out of scope                                                |

Nothing found depends on case-insensitive name matching, on `null` vs `[]` for empty slices, or on
duplicate keys. No HTTP client in the repo decodes into Go slices or maps from an upstream.

## Logging, metrics, tracing, correlation IDs

- **Logging:** `charm.land/log/v2` global logger. The root package `x` (`log.go`) wraps it with
  custom levels `Notice`/`Done`/`Skip` and sets the default level to `NoticeLevel`. `google/`,
  `jsonio/` and `xmlio/` call `charm.land/log` directly. `file/`, `fsio/` and `paths/` use stdlib
  `log`. **No `log/slog` anywhere** (only a comment in `policy/policy.go`).
- **Metrics:** none.
- **Tracing:** none. OpenTelemetry appears only as an indirect dependency of
  `google.golang.org/api`.
- **Correlation IDs on ctx:** none. The repo has no `context.WithValue` and no request-ID
  propagation. `policy.Policy.OnRetry` is the only observability hook.

## Baseline state at HEAD (pre-existing failures)

**Resolved** in `0046cb7` (baseline fix) and `99cadb1` (`go fix`), before Phase 1. The table
records the state Phase 0 found:

| Command               | Result                                                                                                      |
|-----------------------|-------------------------------------------------------------------------------------------------------------|
| `go build ./...`      | **FAIL**: `google/client.go:52` calls `(*transport.Transport).Client`, which `4d1748d` removed               |
| `go vet ./...`        | **FAIL**: `google` (same), `xmlio/xml.go:191` `errors.As(err, eof)` with `*error`, `xmlio/xml.go:104-108` `fPrintF` printf directives |
| `go test -race ./...` | **FAIL**: build failures above, plus `banking/revolut` `TestParseFixture` (0 of 4 txns) and `fsio` `TestGlob_InvalidPattern` |
| `go fix -diff ./...`  | Proposes `reflect.Ptr→reflect.Pointer` and a `Value.Fields()` range loop in `google/gsheets/writer.go`. It cannot analyse `google` until that package builds again |
| `gofmt -l .`          | clean                                                                                                       |
| staticcheck / golangci-lint | not configured                                                                                        |

## Toolchain notes

- `go.mod` already declares `go 1.27`; local toolchain `go1.27.0`, `GOTOOLCHAIN=auto`, no
  `GOEXPERIMENT`/`GODEBUG` set. No upgrade commit was needed.
- Verified the stdlib APIs: `uuid` (`New`, `NewV4`, `NewV7`, `Parse`), `errors.AsType`,
  `encoding/json/v2.UnmarshalRead`.
- `httptest.NewTestServer` takes **`(t testing.TB, handler http.Handler)`**. The example in
  `LAYOUT.md` omits `t`.
- `github.com/google/uuid` is already in `go.sum` as an indirect dependency (via the Google API).
  Fine, as long as no core package imports it.
