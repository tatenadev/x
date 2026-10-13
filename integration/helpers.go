// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
)

// GetJSON sends GET to path (resolved against the base URL) and decodes the
// JSON response into a T.
func (c *Client) GetJSON[T any](ctx context.Context, path string) (T, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		var zero T
		return zero, err
	}
	return c.doJSON[T](ctx, req)
}

// PostJSON sends body as JSON with POST to path and decodes the JSON
// response into a Resp. Use struct{} as Resp to ignore the response.
func (c *Client) PostJSON[Req, Resp any](ctx context.Context, path string, body Req) (Resp, error) {
	return c.sendJSON[Req, Resp](ctx, http.MethodPost, path, body)
}

// PutJSON sends body as JSON with PUT to path and decodes the JSON response
// into a Resp. Use struct{} as Resp to ignore the response.
func (c *Client) PutJSON[Req, Resp any](ctx context.Context, path string, body Req) (Resp, error) {
	return c.sendJSON[Req, Resp](ctx, http.MethodPut, path, body)
}

// sendJSON encodes body up front so the request carries GetBody and can be
// replayed on retry.
func (c *Client) sendJSON[Req, Resp any](ctx context.Context, method, path string, body Req) (Resp, error) {
	var zero Resp
	var buf bytes.Buffer
	if err := c.json.Encode(&buf, body); err != nil {
		return zero, fmt.Errorf("integration: %s: %s: encode request body: %w", c.cfg.Load().Service, method, err)
	}
	// A *bytes.Reader body makes NewRequestWithContext set GetBody and
	// ContentLength.
	req, err := c.newRequest(ctx, method, path, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", c.json.ContentType())
	return c.doJSON[Resp](ctx, req)
}

// doJSON sends req and decodes the response straight from its body, which
// it then closes. An empty response (204, 205, 304, or Content-Length: 0)
// leaves T at its zero value.
func (c *Client) doJSON[T any](ctx context.Context, req *http.Request) (T, error) {
	var zero T
	req.Header.Set("Accept", c.json.ContentType())
	resp, err := c.Do(ctx, req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if !hasBody(resp) {
		return zero, nil
	}
	var v T
	if err := c.json.Decode(resp.Body, &v); err != nil {
		return zero, decodeError(c.cfg.Load(), req, resp, err)
	}
	return v, nil
}

// hasBody reports whether resp may carry a body worth decoding.
func hasBody(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusResetContent, http.StatusNotModified:
		return false
	}
	return resp.ContentLength != 0
}
