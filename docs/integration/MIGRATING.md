# Migrating an Existing Client to the Integration Layer

Phase 7 migrated `google.Client`. This recipe is what that took, generalised, plus notes on the
clients still to go (`browser.Session` and `file.Download`; see `INVENTORY.md`).

## 1. Choose the entry point

| The client…                                                        | Use                                   |
|--------------------------------------------------------------------|---------------------------------------|
| talks to one upstream origin, and our code builds the requests     | `integration.New(service, baseURL, …)` |
| is a third-party SDK that requires an `*http.Client` (many hosts)  | `integration.NewHTTPClient(service, …)` |

Prefer `New`. It resolves paths against the base URL, keeps requests and redirects on that origin,
and turns failures into `*integration.Error`. `NewHTTPClient` runs the same middleware chain but
has none of those guards: its auth transport sends credentials to every host the SDK or a redirect
names. Use it only for SDKs of APIs you trust that much (Google's, for example).

## 2. Move credentials into a `TokenSource`

Domain code must not set `Authorization` itself. Wrap whatever produced the credential:

- **Fixed secrets:** `auth.StaticToken`, `auth.APIKey`, `auth.BasicAuth`.
- **OAuth2 client credentials:** `auth.NewClientCredentials`. It refreshes ahead of expiry,
  deduplicates refreshes, and can be invalidated after a 401.
- **An existing `golang.org/x/oauth2.TokenSource`:** adapt it with a small type in the package that
  already imports `oauth2`. The core must stay stdlib-only, so the adapter can't live there. See
  `oauth2Source` in `google/client.go`. It cannot implement `auth.Invalidator`, because `oauth2`
  caches internally. The client detects that and does not retry 401s, which would only resend the
  same token.

Remove the old `oauth2.Transport` (or hand-set headers) at the same time. Otherwise the request
gets authenticated twice: `Auth` keeps an existing header unless `OverwriteAuth` is set.

## 3. Map the old retry and timeout behaviour onto options

Write down what the old client did, then choose deliberately:

| Old behaviour                        | New option                                                              |
|--------------------------------------|-------------------------------------------------------------------------|
| N retries after the first try        | `WithRetry(N+1, base, max)`: the layer counts **total** attempts        |
| retried plain 500                    | `WithRetryServerErrors()` (only if the upstream documents 500 as transient) |
| retried POST/PATCH on 429/503        | not by default; use `WithIdempotencyKeys()` or `integration.Idempotent(ctx)` only if the upstream honours `Idempotency-Key` |
| no timeout                           | `WithTimeout`, which is required and positive. The attempt timeout covers reading the body, so raise `WithAttemptTimeout` for long transfers |
| no circuit breaker                   | on by default (5 failures / 30s); tune with `WithBreaker`               |

## 4. Check JSON behaviour before switching to `encoding/json/v2`

This only matters when the client decodes JSON itself (the helpers and `codec.JSON` use v2):

- **Name matching:** v2 is case-sensitive; v1 was not. Check that tags match the upstream's exact
  casing.
- **`omitempty`:** v2 omits only empty JSON values (`""`, `null`, `[]`, `{}`). A zero number or
  `false` is now **emitted**. Use `omitzero` where the old shape relied on omission.
- **Nil slices and maps:** v2 encodes them as `[]`/`{}`, not `null`.
- **Duplicate keys:** v2 rejects them. If an upstream really sends them, relax that client only:
  `WithJSONOptions(jsontext.AllowDuplicateNames(true))`, with a comment saying why.

Pin the upstream's payloads as golden files in `testdata/` and decode them in a test, before and
after the switch.

`google.Client` needed none of this: the Google SDK decodes its own JSON. Its golden test only pins
that the new transport hands those bytes through unchanged.

## 5. Test it

- Build the client over a fake base transport (`integrationtest.RecordingTransport`), or over
  `integrationtest.Server`. That helper runs in memory and works inside `synctest`.
- Put every time-dependent test in `synctest.Test`: retries, backoff, timeouts.
- Cover each behaviour change you listed in step 3 with a test that pins the new behaviour.

## 6. List the behaviour changes in the PR

New retries, new timeouts, JSON strictness, and anything that is now *not* retried. Reviewers need
that list more than the diff.

## Remaining clients

### `browser.Session`

It already takes a `Doer`, but `integration.Client.Do` takes a context as its first argument, so
it doesn't satisfy `browser.Doer` directly. A session also follows links to any host, while
`integration.Client` is bound to one origin. Options:

- Create one `integration.Client` per site, and give `browser` a small adapter:
  `doerFunc(func(r *http.Request) (*http.Response, error) { return c.Do(r.Context(), r) })`.
  Note that `Do` turns non-2xx into `*Error`, whereas `Session` currently returns the page with its
  status. The adapter (or `Session`) has to decide which behaviour it wants.
- Or, if a session must roam across hosts, use `NewHTTPClient` without a token source, since a
  scraper has no credentials to leak.

### `file.Download`

It uses bare `http.Get`: no context, no timeout, and an unbounded streaming body. Migrating it
means:

- changing the signature to `Download(ctx, url, path)`, which is a public API change (it has no
  in-repo callers);
- making timeouts that suit large transfers (a long `WithTimeout` and `WithAttemptTimeout`);
- not retrying mid-stream. A partially written file would need resume logic (Range requests) that
  the layer does not provide.

It is a poor fit for the layer until those choices are made. Treat it as its own task.
