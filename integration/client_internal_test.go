// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestNew_Defaults(t *testing.T) {
	c, err := New("billing", "https://api.test/v1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.cfg.Load()
	want := defaultConfig("billing", &url.URL{Scheme: "https", Host: "api.test", Path: "/v1"})
	if cfg.BaseURL.String() != want.BaseURL.String() {
		t.Errorf("BaseURL = %v, want %v", cfg.BaseURL, want.BaseURL)
	}
	got := *cfg
	got.BaseURL, want.BaseURL = nil, nil
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}

	hc, ok := c.doer.(*http.Client)
	if !ok {
		t.Fatalf("doer = %T, want *http.Client", c.doer)
	}
	if hc.Timeout != DefaultTimeout {
		t.Errorf("http.Client.Timeout = %v, want %v", hc.Timeout, DefaultTimeout)
	}
}

func TestNewHTTPTransport(t *testing.T) {
	ht := newHTTPTransport()
	if ht == http.DefaultTransport {
		t.Error("client shares http.DefaultTransport")
	}
	if ht.Protocols == nil || !ht.Protocols.HTTP1() || !ht.Protocols.HTTP2() {
		t.Errorf("Protocols = %v, want HTTP/1 and HTTP/2", ht.Protocols)
	}
	if ht.HTTP2 == nil {
		t.Error("HTTP2Config not set")
	}
}

func TestNew_AttemptTimeout(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want time.Duration
	}{
		{name: "default", want: DefaultAttemptTimeout},
		{name: "lowered to short timeout", opts: []Option{WithTimeout(time.Second)}, want: time.Second},
		{name: "explicit", opts: []Option{WithAttemptTimeout(3 * time.Second)}, want: 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New("billing", "https://api.test", tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.cfg.Load().AttemptTimeout; got != tt.want {
				t.Errorf("AttemptTimeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://api.test/x", "https://api.test/x"},
		{"https://u:p@api.test/x", "https://REDACTED@api.test/x"},
		{"https://api.test/x?token=a&b=1", "https://api.test/x?b=1&token=REDACTED"},
		{"https://api.test/x?client_secret=a&Signature=b&key=c&code=d", "https://api.test/x?Signature=REDACTED&client_secret=REDACTED&code=REDACTED&key=REDACTED"},
		{"https://api.test/x?page=2&sort=asc", "https://api.test/x?page=2&sort=asc"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			u, err := url.Parse(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			before := u.String()
			if got := redactURL(u); got != tt.want {
				t.Errorf("redactURL() = %s, want %s", got, tt.want)
			}
			if u.String() != before {
				t.Errorf("redactURL mutated its input: %s", u)
			}
		})
	}
	if got := redactURL(nil); got != "" {
		t.Errorf("redactURL(nil) = %q", got)
	}
}
