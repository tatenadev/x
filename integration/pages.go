// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"github.com/tatenadev/x/integration/transport"
)

// PageFunc reports the URL of the next page, or "" when the current page
// is the last. It runs after the page's items have been consumed and its
// body closed, so it may inspect headers but not the body. A relative URL
// is resolved against the current page's URL.
type PageFunc func(resp *http.Response) (string, error)

// Pages walks a paginated endpoint lazily. Each page body must be a JSON
// array of T (or null). Items are decoded one at a time as the consumer
// asks for them; a page is fetched only when the consumer reaches it.
//
// The sequence stops when next returns "", when the consumer breaks, or
// after the first error, which is yielded as (zero, err). A nil next means
// a single page. Every page must stay within the base URL's origin.
func (c *Client) Pages[T any](ctx context.Context, path string, next PageFunc) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		req, err := c.newRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			yield(zero, err)
			return
		}
		for req != nil {
			req = c.page[T](ctx, req, next, yield)
		}
	}
}

// page fetches one page and yields its items. It returns the request for
// the next page, or nil when iteration is over: no more pages, the consumer
// stopped, or an error was yielded.
func (c *Client) page[T any](ctx context.Context, req *http.Request, next PageFunc, yield func(T, error) bool) *http.Request {
	var zero T
	req.Header.Set("Accept", c.json.ContentType())
	resp, err := c.Do(ctx, req)
	if err != nil {
		yield(zero, err)
		return nil
	}
	defer resp.Body.Close()

	if hasBody(resp) {
		for v, err := range c.json.Each[T](resp.Body) {
			if err != nil {
				yield(zero, decodeError(c.cfg.Load(), req, resp, err))
				return nil
			}
			if !yield(v, nil) {
				return nil
			}
		}
	}
	if next == nil {
		return nil
	}

	raw, err := next(resp)
	if err != nil {
		yield(zero, fmt.Errorf("integration: %s: next page: %w", c.cfg.Load().Service, err))
		return nil
	}
	if raw == "" {
		return nil
	}
	nreq, err := nextPageRequest(ctx, req, resp, raw)
	if err != nil {
		yield(zero, fmt.Errorf("integration: %s: %w", c.cfg.Load().Service, err))
		return nil
	}
	return nreq
}

// nextPageRequest builds the request for raw, resolved against the URL the
// current page was actually served from.
func nextPageRequest(ctx context.Context, req *http.Request, resp *http.Response, raw string) (*http.Request, error) {
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("next page URL: %w", unwrapURLError(err))
	}
	cur := req.URL
	if resp.Request != nil && resp.Request.URL != nil {
		cur = resp.Request.URL
	}
	u := cur.ResolveReference(ref)
	u.Fragment, u.RawFragment = "", ""
	if u.String() == cur.String() {
		return nil, fmt.Errorf("next page URL %s repeats the current page", transport.RedactURL(u))
	}
	nreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("next page request: %w", unwrapURLError(err))
	}
	return nreq, nil
}

// LinkHeaderNext is a [PageFunc] that follows the rel="next" target of the
// response's RFC 8288 Link header(s).
func LinkHeaderNext(resp *http.Response) (string, error) {
	for _, h := range resp.Header.Values("Link") {
		for h = strings.TrimSpace(h); h != ""; h = strings.TrimSpace(h) {
			end := strings.IndexByte(h, '>')
			if h[0] != '<' || end < 0 {
				return "", fmt.Errorf("malformed Link header")
			}
			target := h[1:end]
			var params string
			params, h = cutLinkParams(h[end+1:])
			if linkRelHas(params, "next") {
				return target, nil
			}
		}
	}
	return "", nil
}

// cutLinkParams splits s at the first comma outside a quoted string.
func cutLinkParams(s string) (params, rest string) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '\\':
			if inQuote {
				i++
			}
		case ',':
			if !inQuote {
				return s[:i], s[i+1:]
			}
		}
	}
	return s, ""
}

// linkRelHas reports whether a link's parameters include rel=want. rel may
// list several space-separated relation types.
func linkRelHas(params, want string) bool {
	for p := range strings.SplitSeq(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "rel") {
			continue
		}
		for rel := range strings.FieldsSeq(strings.Trim(strings.TrimSpace(v), `"`)) {
			if strings.EqualFold(rel, want) {
				return true
			}
		}
	}
	return false
}
