package transport

import (
	"net/http"
	"slices"
)

// Middleware wraps a single round trip: it receives the request and
// the next [roundTrip] in the chain. A Middleware must eventually
// call next to complete the request, unless it fails fast (for example, a
// rate limiter whose context is already done). Per [http.RoundTripper] it
// must not modify req — clone before changing anything on it — and, on any
// path that returns an error without calling next, must close the request's
// body itself (see [closeBody]), since nothing downstream will.
type Middleware func(RoundTrip) RoundTrip

// RoundTrip is one hop of the chain: a function that completes a round
// trip. It satisfies [http.RoundTripper], so it also serves as a base
// transport via [WithBaseTransport].
type RoundTrip func(*http.Request) (*http.Response, error)

// closeBody closes r.Body, if any. A [Middleware] that returns an
// error without calling next must call this: net/http's Client assumes the
// RoundTripper it invoked already closed the request body on any error
// path, and will not close it itself.
func closeBody(r *http.Request) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
}

func chain(inner RoundTrip, mw ...Middleware) RoundTrip {
	for _, m := range slices.Backward(mw) {
		next := inner // captured per iteration; rtf is reassigned below
		inner = func(req *http.Request) (*http.Response, error) {
			return m(next)(req)
		}
	}
	return inner
}
