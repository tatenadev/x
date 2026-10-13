// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

// Package integrationtest provides fakes for testing code built on package
// integration: a [RecordingTransport] to pass to
// integration.WithBaseTransport, a [FakeDoer] for services that depend on a
// Doer, and a [Body] that records whether it was closed.
package integrationtest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// RespondFunc produces the response for one request.
type RespondFunc func(*http.Request) (*http.Response, error)

// recorder keeps the requests seen by a fake. It is safe for concurrent use.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
}

// Requests returns the requests received so far, in arrival order.
func (r *recorder) Requests() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*http.Request(nil), r.reqs...)
}

// respond runs fn, defaulting to an empty 200, and points the response
// back at its request the way http.Transport does.
func respond(fn RespondFunc, req *http.Request) (*http.Response, error) {
	if fn == nil {
		fn = func(*http.Request) (*http.Response, error) { return Response(http.StatusOK, ""), nil }
	}
	resp, err := fn(req)
	if resp != nil && resp.Request == nil {
		resp.Request = req
	}
	return resp, err
}

// RecordingTransport is an [http.RoundTripper] that records each request
// and answers with Respond (an empty 200 when nil).
type RecordingTransport struct {
	recorder
	Respond RespondFunc
}

// RoundTrip records req and returns Respond(req).
func (t *RecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.record(req)
	return respond(t.Respond, req)
}

// FakeDoer is an integration.Doer that records each request and answers
// with Respond (an empty 200 when nil).
type FakeDoer struct {
	recorder
	Respond RespondFunc
}

// Do records req and returns Respond(req).
func (d *FakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.record(req)
	return respond(d.Respond, req)
}

// Body is a response or request body that records Close calls.
type Body struct {
	io.Reader
	closed atomic.Int32
}

// NewBody returns a Body that reads s.
func NewBody(s string) *Body {
	return &Body{Reader: strings.NewReader(s)}
}

// Close records the call. It never fails.
func (b *Body) Close() error {
	b.closed.Add(1)
	return nil
}

// Closed reports whether Close was called at least once.
func (b *Body) Closed() bool { return b.closed.Load() > 0 }

// Response builds a response with the given status and a [*Body] reading
// body. Retrieve the Body with resp.Body.(*integrationtest.Body).
func Response(status int, body string) *http.Response {
	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          NewBody(body),
		ContentLength: int64(len(body)),
	}
}

// Script returns a RespondFunc that answers the nth request with steps[n].
// Once the steps run out, the last one repeats. It is safe for concurrent
// use.
func Script(steps ...RespondFunc) RespondFunc {
	var n atomic.Int64
	return func(req *http.Request) (*http.Response, error) {
		i := min(int(n.Add(1))-1, len(steps)-1)
		return steps[i](req)
	}
}

// Status returns a RespondFunc answering with the given status and body.
func Status(code int, body string) RespondFunc {
	return func(*http.Request) (*http.Response, error) {
		return Response(code, body), nil
	}
}

// Fail returns a RespondFunc failing with err.
func Fail(err error) RespondFunc {
	return func(*http.Request) (*http.Response, error) {
		return nil, err
	}
}

// Server starts an in-memory test server for h (httptest.NewTestServer),
// so it works inside a synctest bubble, and closes it when t ends. Its
// URL is set and srv.Client() is ready to use.
func Server(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	srv.Client() // starts the server and sets srv.URL
	t.Cleanup(srv.Close)
	return srv
}
