// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tatenadev/x/integration/codec"
)

// Defaults for [NewClientCredentials].
const (
	DefaultSkew         = 30 * time.Second
	DefaultFetchTimeout = 30 * time.Second
)

// maxTokenErrorBody caps how much of a token endpoint error body is read.
const maxTokenErrorBody = 4 << 10

// Doer sends the token request. *http.Client satisfies it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ClientCredentials is a [TokenSource] for the OAuth 2.0 client
// credentials grant (RFC 6749 §4.4). It caches the token, refreshes it
// once it is within the skew of expiry, and runs at most one refresh at a
// time: concurrent callers wait for and share its result. A refresh is not
// tied to any caller's cancellation, so one impatient caller cannot fail
// the others. It implements [Invalidator].
type ClientCredentials struct {
	tokenURL     string
	clientID     string
	clientSecret string
	scopes       []string
	params       url.Values
	authInBody   bool
	skew         time.Duration
	fetchTimeout time.Duration
	doer         Doer

	mu       sync.Mutex
	tok      Token
	inflight *refresh // nil when no refresh is running
}

// refresh is one in-flight token request, shared by every caller that
// needs a token while it runs. done is closed after tok and err are set.
type refresh struct {
	done chan struct{}
	tok  Token
	err  error
}

// CredentialsOption configures [NewClientCredentials].
type CredentialsOption func(*ClientCredentials) error

// WithScopes sets the requested scopes.
func WithScopes(scopes ...string) CredentialsOption {
	return func(c *ClientCredentials) error {
		c.scopes = append([]string(nil), scopes...)
		return nil
	}
}

// WithEndpointParams adds parameters to the token request, such as an
// "audience". They cannot replace grant_type, scope or the client
// credentials.
func WithEndpointParams(params url.Values) CredentialsOption {
	return func(c *ClientCredentials) error {
		for k := range params {
			switch k {
			case "grant_type", "scope", "client_id", "client_secret":
				return fmt.Errorf("auth: endpoint param %q is managed by ClientCredentials", k)
			}
		}
		c.params = params.Clone()
		return nil
	}
}

// WithAuthInBody sends client_id and client_secret in the request body
// instead of HTTP Basic auth, for providers that require it.
func WithAuthInBody() CredentialsOption {
	return func(c *ClientCredentials) error {
		c.authInBody = true
		return nil
	}
}

// WithSkew sets how long before expiry a token is refreshed (default 30s).
func WithSkew(d time.Duration) CredentialsOption {
	return func(c *ClientCredentials) error {
		if d < 0 {
			return fmt.Errorf("auth: skew %v is negative", d)
		}
		c.skew = d
		return nil
	}
}

// WithFetchTimeout bounds a single token request (default 30s).
func WithFetchTimeout(d time.Duration) CredentialsOption {
	return func(c *ClientCredentials) error {
		if d <= 0 {
			return fmt.Errorf("auth: fetch timeout %v is not positive", d)
		}
		c.fetchTimeout = d
		return nil
	}
}

// WithDoer sets the client used to reach the token endpoint. The default
// is a dedicated *http.Client.
func WithDoer(d Doer) CredentialsOption {
	return func(c *ClientCredentials) error {
		if d == nil {
			return errors.New("auth: doer is nil")
		}
		c.doer = d
		return nil
	}
}

// NewClientCredentials returns a token source for clientID at tokenURL.
func NewClientCredentials(tokenURL, clientID, clientSecret string, opts ...CredentialsOption) (*ClientCredentials, error) {
	u, err := url.Parse(tokenURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("auth: token URL must be an absolute http(s) URL")
	}
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("auth: client ID and secret are required")
	}
	c := &ClientCredentials{
		tokenURL:     u.String(),
		clientID:     clientID,
		clientSecret: clientSecret,
		skew:         DefaultSkew,
		fetchTimeout: DefaultFetchTimeout,
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	if c.doer == nil {
		c.doer = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}
	return c, nil
}

// Token returns the cached token, or waits for a refresh when the cached
// one is missing, invalidated, or within the skew of expiry. If ctx ends
// first, Token returns context.Cause(ctx) and the refresh carries on for
// the other callers.
func (c *ClientCredentials) Token(ctx context.Context) (Token, error) {
	c.mu.Lock()
	if c.usable(time.Now()) {
		tok := c.tok
		c.mu.Unlock()
		return tok, nil
	}
	r := c.inflight
	if r == nil {
		r = &refresh{done: make(chan struct{})}
		c.inflight = r
		// Keep ctx values (tracing) but not its cancellation.
		go c.run(context.WithoutCancel(ctx), r)
	}
	c.mu.Unlock()

	select {
	case <-r.done:
		return r.tok, r.err
	case <-ctx.Done():
		return Token{}, context.Cause(ctx)
	}
}

// Invalidate drops the cached token so the next Token call refreshes.
func (c *ClientCredentials) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tok = Token{}
}

// usable reports whether the cached token can be handed out at now.
// c.mu must be held.
func (c *ClientCredentials) usable(now time.Time) bool {
	if c.tok.Value == "" {
		return false
	}
	return c.tok.Expiry.IsZero() || now.Before(c.tok.Expiry.Add(-c.skew))
}

// run performs refresh r and publishes its result. Errors are not cached:
// the next caller starts a new refresh.
func (c *ClientCredentials) run(ctx context.Context, r *refresh) {
	r.tok, r.err = c.fetch(ctx)

	c.mu.Lock()
	if r.err == nil {
		c.tok = r.tok
	}
	c.inflight = nil
	c.mu.Unlock()
	close(r.done)
}

// tokenResponse is the RFC 6749 §5.1 success body.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	// Some providers send expires_in as a string; accept both.
	ExpiresIn jsontext.Value `json:"expires_in,omitzero"`
}

// tokenErrorResponse is the RFC 6749 §5.2 error body.
type tokenErrorResponse struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (c *ClientCredentials) fetch(ctx context.Context) (Token, error) {
	ctx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
	defer cancel()

	form := url.Values{"grant_type": {"client_credentials"}}
	if len(c.scopes) > 0 {
		form.Set("scope", strings.Join(c.scopes, " "))
	}
	maps.Copy(form, c.params)
	if c.authInBody {
		form.Set("client_id", c.clientID)
		form.Set("client_secret", c.clientSecret)
	}
	var body bytes.Buffer
	if err := (codec.Form{}).Encode(&body, form); err != nil {
		return Token{}, fmt.Errorf("auth: client credentials: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return Token{}, fmt.Errorf("auth: client credentials: %w", err)
	}
	req.Header.Set("Content-Type", codec.ContentTypeForm)
	req.Header.Set("Accept", codec.ContentTypeJSON)
	if !c.authInBody {
		// RFC 6749 §2.3.1: form-encode the credentials before Basic.
		req.SetBasicAuth(url.QueryEscape(c.clientID), url.QueryEscape(c.clientSecret))
	}

	issued := time.Now() // count expiry from before the round trip
	resp, err := c.doer.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return Token{}, fmt.Errorf("auth: client credentials: token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Token{}, tokenEndpointError(resp)
	}
	var tr tokenResponse
	if err := (codec.JSON{}).Decode(resp.Body, &tr); err != nil {
		return Token{}, fmt.Errorf("auth: client credentials: decode token response: %w", err)
	}
	return tr.token(issued)
}

func (tr tokenResponse) token(issued time.Time) (Token, error) {
	if tr.AccessToken == "" {
		return Token{}, errors.New("auth: client credentials: response has no access_token")
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "Bearer") {
		return Token{}, fmt.Errorf("auth: client credentials: unsupported token_type %q", tr.TokenType)
	}
	tok := Token{Type: "Bearer", Value: tr.AccessToken}
	secs, err := parseExpiresIn(tr.ExpiresIn)
	if err != nil {
		return Token{}, fmt.Errorf("auth: client credentials: %w", err)
	}
	if secs > 0 {
		tok.Expiry = issued.Add(time.Duration(secs) * time.Second)
	}
	return tok, nil
}

// parseExpiresIn accepts a JSON number or a string holding one. Absent
// means no known expiry (0).
func parseExpiresIn(v jsontext.Value) (int64, error) {
	var raw string
	switch v.Kind() {
	case jsontext.KindInvalid, jsontext.KindNull:
		return 0, nil
	case jsontext.KindNumber:
		raw = string(v)
	case jsontext.KindString:
		if err := json.Unmarshal(v, &raw); err != nil {
			return 0, fmt.Errorf("expires_in: %w", err)
		}
	default:
		return 0, fmt.Errorf("expires_in has unexpected kind %v", v.Kind())
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("expires_in %q is not a non-negative integer", raw)
	}
	return n, nil
}

// tokenEndpointError describes a non-2xx token response using its RFC 6749
// error code when present. The client secret is never part of it.
func tokenEndpointError(resp *http.Response) error {
	msg := fmt.Sprintf("auth: client credentials: token endpoint returned %s", resp.Status)
	var te tokenErrorResponse
	if json.UnmarshalRead(io.LimitReader(resp.Body, maxTokenErrorBody), &te) == nil && te.Error != "" {
		msg += ": " + te.Error
		if te.Description != "" {
			msg += ": " + te.Description
		}
	}
	return errors.New(msg)
}

// String formats c without its secret or token.
func (c *ClientCredentials) String() string {
	return fmt.Sprintf("auth.ClientCredentials{TokenURL: %s, ClientID: %s}", c.tokenURL, c.clientID)
}

// GoString formats c for %#v without its secret or token.
func (c *ClientCredentials) GoString() string { return c.String() }

// LogValue logs c without its secret or token.
func (c *ClientCredentials) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token_url", c.tokenURL), slog.String("client_id", c.clientID))
}
