// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"maps"
	"net/http"
	"slices"

	"golang.org/x/oauth2"

	"github.com/tatenadev/x/google/gdrive"
	"github.com/tatenadev/x/google/gsheets"
	"github.com/tatenadev/x/google/oauth"
	"github.com/tatenadev/x/paths"
	"github.com/tatenadev/x/policy"
	"github.com/tatenadev/x/transport"
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

	return &http.Client{Transport: transport.New(
		transport.WithBaseTransport((&oauth2.Transport{Source: ts}).RoundTrip),
		transport.WithMiddleware(
			transport.Retry(transport.Retrier{Backoff: policy.DefaultBackoff}),
		),
	)}, nil
}
