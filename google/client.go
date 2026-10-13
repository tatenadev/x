// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/tatenadev/x/google/gdrive"
	"github.com/tatenadev/x/google/gsheets"
	"github.com/tatenadev/x/google/oauth"
	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/paths"
)

// Transport settings for Google APIs.
const (
	// clientTimeout bounds one call, retries included. Drive media
	// transfers can be long, so this is far above the layer's 30s default.
	clientTimeout = 10 * time.Minute
	// maxAttempts and the backoff bounds match the previous client
	// (policy.DefaultBackoff: three retries, 200ms doubling to 10s).
	maxAttempts = 4
	backoffBase = 200 * time.Millisecond
	backoffMax  = 10 * time.Second
)

func fullScope() []string {
	scopes := make(map[string]struct{})
	for _, s := range gsheets.ScopesAll {
		scopes[s] = struct{}{}
	}
	for _, s := range oauth.ScopesAll {
		scopes[s] = struct{}{}
	}
	for _, s := range gdrive.ScopeFull {
		scopes[s] = struct{}{}
	}
	return slices.Sorted(maps.Keys(scopes))
}

// Client returns an [http.Client] that uses the provided context and credentials to authenticate all requests.
// If no credentials are provided, the default credentials from the environment are used.
func Client(ctx context.Context, xdg paths.XDG, scope ...string) (*http.Client, error) {
	if len(scope) == 0 {
		scope = fullScope()
	}
	ts, err := TokenSource(ctx, xdg, scope)
	if err != nil {
		return nil, err
	}
	return newClient(ts)
}

// newClient builds the HTTP client for the Google API libraries on the
// integration layer. They need a plain *http.Client and call several Google
// hosts, hence NewHTTPClient rather than an origin-bound integration.Client.
func newClient(ts oauth2.TokenSource, opts ...integration.Option) (*http.Client, error) {
	base := []integration.Option{
		integration.WithTokenSource(oauth2Source{ts}),
		integration.WithTimeout(clientTimeout),
		integration.WithAttemptTimeout(clientTimeout),
		integration.WithRetry(maxAttempts, backoffBase, backoffMax),
		// Google asks clients to retry 500 ("backendError") as well.
		integration.WithRetryServerErrors(),
	}
	hc, err := integration.NewHTTPClient("google", append(base, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("google: %w", err)
	}
	return hc, nil
}

// oauth2Source adapts an oauth2.TokenSource to auth.TokenSource. It cannot
// be invalidated (oauth2 caches internally), so a 401 is not retried.
type oauth2Source struct {
	ts oauth2.TokenSource
}

func (s oauth2Source) Token(context.Context) (auth.Token, error) {
	tok, err := s.ts.Token()
	if err != nil {
		return auth.Token{}, err
	}
	if !strings.EqualFold(tok.Type(), "Bearer") {
		return auth.Token{}, fmt.Errorf("google: unsupported token type %q", tok.Type())
	}
	return auth.Token{Type: "Bearer", Value: tok.AccessToken, Expiry: tok.Expiry}, nil
}
