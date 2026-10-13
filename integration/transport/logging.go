// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// maxLoggedBody caps how much of a body is captured for a log line.
const maxLoggedBody = 4 << 10

// LoggingOption configures [Logging].
type LoggingOption func(*loggingOptions)

type loggingOptions struct {
	headers, bodies bool
}

// LogHeaders adds request and response headers to the call log line, with
// credential headers redacted. Headers set by Auth are never seen: Logging
// runs outside it.
func LogHeaders() LoggingOption { return func(o *loggingOptions) { o.headers = true } }

// LogBodies adds up to 4 KiB of the request and response bodies to the
// call log line. Only JSON and form bodies are shown, with sensitive
// members and fields redacted; other types are noted, not shown. A request
// body is captured only when it can be replayed via GetBody.
func LogBodies() LoggingOption { return func(o *loggingOptions) { o.bodies = true } }

// Logging returns middleware that writes one log line per logical call, at
// Info, or Warn on a transport error or 5xx. It sits outside Retry and
// adds a Debug line per attempt. Log calls go through the *Context slog
// methods so handlers can pick trace and request IDs from the context.
// URLs are redacted; headers and bodies are off unless enabled.
func Logging(l *slog.Logger, service string, opts ...LoggingOption) Middleware {
	var o loggingOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			ctx := req.Context()
			route := Route(req)
			var attempts atomic.Int32
			actx := ObserveAttempts(ctx, func(a AttemptInfo) {
				attempts.Add(1)
				if !l.Enabled(ctx, slog.LevelDebug) {
					return
				}
				attrs := []slog.Attr{
					slog.String("service", service),
					slog.String("method", req.Method),
					slog.String("route", route),
					slog.Int("attempt", a.Attempt),
					slog.Int("status", a.StatusCode),
					slog.Duration("duration", a.Duration),
					slog.Bool("retry", a.Retry),
				}
				if a.Retry {
					attrs = append(attrs, slog.Duration("delay", a.Delay))
				}
				if a.Err != nil {
					attrs = append(attrs, slog.String("error", a.Err.Error()))
				}
				l.LogAttrs(ctx, slog.LevelDebug, "integration attempt", attrs...)
			})

			var reqBody string
			if o.bodies {
				reqBody = captureRequestBody(req)
			}

			start := time.Now()
			resp, err := next.RoundTrip(req.WithContext(actx))
			duration := time.Since(start)

			attrs := []slog.Attr{
				slog.String("service", service),
				slog.String("method", req.Method),
				slog.String("route", route),
				slog.String("url", RedactURL(req.URL)),
				slog.Int("status", statusOf(resp)),
				slog.Int("attempts", max(int(attempts.Load()), 1)),
				slog.Duration("duration", duration),
				slog.String("request_id", requestIDOf(req)),
			}
			if err != nil {
				attrs = append(attrs, slog.String("error", err.Error()))
			}
			if o.headers {
				attrs = append(attrs, slog.Any("request_headers", RedactHeader(req.Header)))
				if resp != nil {
					attrs = append(attrs, slog.Any("response_headers", RedactHeader(resp.Header)))
				}
			}
			if o.bodies {
				if reqBody != "" {
					attrs = append(attrs, slog.String("request_body", reqBody))
				}
				if resp != nil {
					attrs = append(attrs, slog.String("response_body", captureResponseBody(resp)))
				}
			}

			level := slog.LevelInfo
			if err != nil || resp.StatusCode >= http.StatusInternalServerError {
				level = slog.LevelWarn
			}
			l.LogAttrs(ctx, level, "integration call", attrs...)
			return resp, err
		})
	}
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func requestIDOf(req *http.Request) string {
	if id := req.Header.Get(HeaderRequestID); id != "" {
		return id
	}
	id, _ := RequestIDFromContext(req.Context())
	return id
}

// captureRequestBody reads a replayable request body prefix without
// consuming the body that will be sent.
func captureRequestBody(req *http.Request) string {
	if req.GetBody == nil || req.Body == nil || req.Body == http.NoBody {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	read, err := readPrefix(rc)
	b, truncated := shown(read)
	return redactBody(req.Header.Get("Content-Type"), b, truncated || err != nil)
}

// captureResponseBody reads a prefix of resp.Body for the log and puts
// every byte it read back in front of the rest, so the caller still reads
// the whole body (and any read error).
func captureResponseBody(resp *http.Response) string {
	if resp.Body == nil || resp.Body == http.NoBody {
		return ""
	}
	read, err := readPrefix(resp.Body)
	rest := io.Reader(resp.Body)
	if err != nil {
		rest = errReader{err}
	}
	resp.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(read), rest), Closer: resp.Body}
	b, truncated := shown(read)
	return redactBody(resp.Header.Get("Content-Type"), b, truncated || err != nil)
}

// readPrefix reads up to one byte more than is logged, so that shown can
// tell whether the body was cut.
func readPrefix(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, maxLoggedBody+1))
}

// shown returns the part of read that is logged and whether it was cut.
func shown(read []byte) ([]byte, bool) {
	if len(read) > maxLoggedBody {
		return read[:maxLoggedBody], true
	}
	return read, false
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type prefixedBody struct {
	io.Reader
	io.Closer
}
