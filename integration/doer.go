// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

// Package integration is the outbound HTTP integration layer: a [Client]
// bound to one upstream service, with a validated [Config], a middleware
// chain (see package [github.com/tatenadev/x/integration/transport]) and
// typed [Error] values for failed calls.
//
// Domain services depend on *Client (built with a fake base transport in
// tests) or on a small interface they define themselves, never on
// *http.Client.
package integration

import "net/http"

// Doer sends one HTTP request and returns its response. *http.Client
// satisfies it. Doer is deliberately non-generic: generic methods cannot
// satisfy an interface, so mocking happens at this level, not by faking
// [Client].
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}
