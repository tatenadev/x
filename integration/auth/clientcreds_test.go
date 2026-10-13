// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
)

// idp is a fake token endpoint. It issues "tok-N" for the Nth request.
type idp struct {
	requests  atomic.Int32
	expiresIn string // raw JSON for expires_in; "" omits it
	status    int
	errBody   string
	hold      chan struct{} // when set, each request waits for it to close
	lastForm  url.Values
	lastAuth  string
	mu        sync.Mutex
}

func (p *idp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := p.requests.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.lastForm, p.lastAuth = r.PostForm, r.Header.Get("Authorization")
	p.mu.Unlock()
	if p.hold != nil {
		<-p.hold
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
		io.WriteString(w, p.errBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	body := fmt.Sprintf(`{"access_token":"tok-%d","token_type":"bearer"`, n)
	if p.expiresIn != "" {
		body += `,"expires_in":` + p.expiresIn
	}
	io.WriteString(w, body+"}")
}

func (p *idp) form() (url.Values, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastForm, p.lastAuth
}

func newCC(t *testing.T, p *idp, opts ...auth.CredentialsOption) *auth.ClientCredentials {
	t.Helper()
	srv := integrationtest.Server(t, p)
	cc, err := auth.NewClientCredentials(srv.URL+"/token", "my client", "s3cret&",
		append([]auth.CredentialsOption{auth.WithDoer(srv.Client())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func mustToken(t *testing.T, src auth.TokenSource) auth.Token {
	t.Helper()
	tok, err := src.Token(t.Context())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	return tok
}

func TestClientCredentials_Request(t *testing.T) {
	tests := []struct {
		name     string
		opts     []auth.CredentialsOption
		wantForm url.Values
		wantAuth string
	}{
		{
			name:     "basic auth, form-encoded credentials",
			opts:     []auth.CredentialsOption{auth.WithScopes("read", "write")},
			wantForm: url.Values{"grant_type": {"client_credentials"}, "scope": {"read write"}},
			// base64("my+client:s3cret%26")
			wantAuth: "Basic bXkrY2xpZW50OnMzY3JldCUyNg==",
		},
		{
			name: "credentials in body with extra params",
			opts: []auth.CredentialsOption{auth.WithAuthInBody(), auth.WithEndpointParams(url.Values{"audience": {"api"}})},
			wantForm: url.Values{"grant_type": {"client_credentials"}, "audience": {"api"},
				"client_id": {"my client"}, "client_secret": {"s3cret&"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &idp{expiresIn: "3600"}
			tok := mustToken(t, newCC(t, p, tt.opts...))
			if tok.Type != "Bearer" || tok.Value != "tok-1" {
				t.Errorf("token = %#v (%s)", tok, tok.Value)
			}
			form, authz := p.form()
			if fmt.Sprint(form) != fmt.Sprint(tt.wantForm) {
				t.Errorf("form = %v, want %v", form, tt.wantForm)
			}
			if authz != tt.wantAuth {
				t.Errorf("Authorization = %q, want %q", authz, tt.wantAuth)
			}
		})
	}
}

func TestClientCredentials_ExpiresIn(t *testing.T) {
	tests := []struct {
		raw     string
		want    time.Duration // 0 = no expiry
		wantErr bool
	}{
		{raw: "3600", want: time.Hour},
		{raw: `"3600"`, want: time.Hour}, // some providers quote it
		{raw: "", want: 0},
		{raw: "null", want: 0},
		{raw: `"soon"`, wantErr: true},
		{raw: "-5", wantErr: true},
		{raw: "1.5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				tok, err := newCC(t, &idp{expiresIn: tt.raw}).Token(t.Context())
				if tt.wantErr {
					if err == nil {
						t.Fatal("Token() error = nil")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var got time.Duration
				if !tok.Expiry.IsZero() {
					got = tok.Expiry.Sub(start)
				}
				if got != tt.want {
					t.Errorf("expires after %v, want %v", got, tt.want)
				}
			})
		})
	}
}

func TestClientCredentials_CachesUntilSkew(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &idp{expiresIn: "3600"}
		cc := newCC(t, p, auth.WithSkew(30*time.Second))

		if got := mustToken(t, cc).Value; got != "tok-1" {
			t.Fatalf("first token = %s", got)
		}
		// Just before expiry-skew: still cached.
		synctest.Sleep(time.Hour - 30*time.Second - time.Second)
		if got := mustToken(t, cc).Value; got != "tok-1" || p.requests.Load() != 1 {
			t.Fatalf("token = %s after %d requests; want cached tok-1", got, p.requests.Load())
		}
		// Past expiry-skew, though the token has not expired yet: refreshed.
		synctest.Sleep(2 * time.Second)
		if got := mustToken(t, cc).Value; got != "tok-2" || p.requests.Load() != 2 {
			t.Fatalf("token = %s after %d requests; want refreshed tok-2", got, p.requests.Load())
		}
	})
}

func TestClientCredentials_ConcurrentRefreshDeduped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &idp{expiresIn: "60"}
		cc := newCC(t, p)
		mustToken(t, cc)
		synctest.Sleep(time.Minute) // expired

		p.hold = make(chan struct{})
		const callers = 100
		var wg sync.WaitGroup
		tokens := make([]string, callers)
		errs := make([]error, callers)
		for i := range callers {
			wg.Go(func() {
				tok, err := cc.Token(t.Context())
				tokens[i], errs[i] = tok.Value, err
			})
		}
		// Every caller is now blocked: one refresh is held at the IdP and
		// the rest wait on it.
		synctest.Wait()
		close(p.hold)
		wg.Wait()

		if n := p.requests.Load(); n != 2 {
			t.Errorf("token endpoint saw %d requests, want 2 (initial + one refresh)", n)
		}
		for i := range callers {
			if errs[i] != nil || tokens[i] != "tok-2" {
				t.Fatalf("caller %d got %q, %v; want tok-2", i, tokens[i], errs[i])
			}
		}
	})
}

func TestClientCredentials_ConcurrentRace(t *testing.T) {
	// Real time and real goroutine scheduling, for -race.
	p := &idp{expiresIn: "3600"}
	cc := newCC(t, p)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if _, err := cc.Token(t.Context()); err != nil {
				t.Error(err)
			}
			cc.Invalidate()
		})
	}
	wg.Wait()
	if n := p.requests.Load(); n < 1 || n > 100 {
		t.Errorf("token endpoint saw %d requests", n)
	}
}

func TestClientCredentials_Invalidate(t *testing.T) {
	p := &idp{expiresIn: "3600"}
	cc := newCC(t, p)
	mustToken(t, cc)
	mustToken(t, cc)
	cc.Invalidate()
	if got := mustToken(t, cc).Value; got != "tok-2" || p.requests.Load() != 2 {
		t.Errorf("after Invalidate: token %s, %d requests; want tok-2, 2", got, p.requests.Load())
	}
}

func TestClientCredentials_CallerCancelDoesNotFailOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &idp{expiresIn: "3600", hold: make(chan struct{})}
		cc := newCC(t, p)

		cause := errors.New("caller gave up")
		ctx, cancel := context.WithCancelCause(t.Context())
		var wg sync.WaitGroup
		var impatientErr, patientErr error
		var patientTok auth.Token
		wg.Go(func() { _, impatientErr = cc.Token(ctx) })
		wg.Go(func() { patientTok, patientErr = cc.Token(t.Context()) })
		synctest.Wait()

		cancel(cause)
		synctest.Wait()
		if !errors.Is(impatientErr, cause) {
			t.Errorf("canceled caller error = %v, want its context cause", impatientErr)
		}
		close(p.hold)
		wg.Wait()
		if patientErr != nil || patientTok.Value != "tok-1" || p.requests.Load() != 1 {
			t.Errorf("other caller got %q, %v after %d requests", patientTok.Value, patientErr, p.requests.Load())
		}
	})
}

func TestClientCredentials_Errors(t *testing.T) {
	tests := []struct {
		name string
		p    *idp
		want string
	}{
		{name: "oauth error", p: &idp{status: http.StatusUnauthorized,
			errBody: `{"error":"invalid_client","error_description":"unknown client"}`},
			want: "401 Unauthorized: invalid_client: unknown client"},
		{name: "plain error", p: &idp{status: http.StatusBadGateway, errBody: "upstream down"}, want: "502 Bad Gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := newCC(t, tt.p)
			_, err := cc.Token(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Token() error = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error leaks the client secret: %v", err)
			}
			// Errors are not cached: the next call asks again.
			cc.Token(t.Context())
			if n := tt.p.requests.Load(); n != 2 {
				t.Errorf("requests = %d, want 2", n)
			}
		})
	}
}

func TestClientCredentials_BadResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "no access token", body: `{"token_type":"bearer"}`},
		{name: "unsupported type", body: `{"access_token":"a","token_type":"mac"}`},
		{name: "duplicate key", body: `{"access_token":"a","access_token":"b"}`},
		{name: "not json", body: `access_token=a`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tt.body)
			}))
			cc, err := auth.NewClientCredentials(srv.URL, "id", "secret", auth.WithDoer(srv.Client()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cc.Token(t.Context()); err == nil {
				t.Error("Token() error = nil")
			}
		})
	}
}

func TestNewClientCredentials_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		tokenURL string
		id, sec  string
		opts     []auth.CredentialsOption
	}{
		{name: "relative URL", tokenURL: "/token", id: "id", sec: "sec"},
		{name: "bad scheme", tokenURL: "ftp://idp.test/token", id: "id", sec: "sec"},
		{name: "no id", tokenURL: "https://idp.test/token", sec: "sec"},
		{name: "no secret", tokenURL: "https://idp.test/token", id: "id"},
		{name: "negative skew", tokenURL: "https://idp.test/token", id: "id", sec: "sec",
			opts: []auth.CredentialsOption{auth.WithSkew(-time.Second)}},
		{name: "zero fetch timeout", tokenURL: "https://idp.test/token", id: "id", sec: "sec",
			opts: []auth.CredentialsOption{auth.WithFetchTimeout(0)}},
		{name: "managed param", tokenURL: "https://idp.test/token", id: "id", sec: "sec",
			opts: []auth.CredentialsOption{auth.WithEndpointParams(url.Values{"grant_type": {"password"}})}},
		{name: "nil doer", tokenURL: "https://idp.test/token", id: "id", sec: "sec",
			opts: []auth.CredentialsOption{auth.WithDoer(nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := auth.NewClientCredentials(tt.tokenURL, tt.id, tt.sec, tt.opts...); err == nil {
				t.Error("NewClientCredentials() error = nil")
			}
		})
	}
}
