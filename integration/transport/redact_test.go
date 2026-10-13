// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://api.test/x", "https://api.test/x"},
		{"https://u:p@api.test/x", "https://REDACTED@api.test/x"},
		{"https://api.test/x?token=a&b=1", "https://api.test/x?b=1&token=REDACTED"},
		{"https://api.test/x?client_secret=a&Signature=b&key=c&code=d",
			"https://api.test/x?Signature=REDACTED&client_secret=REDACTED&code=REDACTED&key=REDACTED"},
		{"https://api.test/x?page=2&sort=asc", "https://api.test/x?page=2&sort=asc"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			u, err := url.Parse(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			before := u.String()
			if got := RedactURL(u); got != tt.want {
				t.Errorf("RedactURL() = %s, want %s", got, tt.want)
			}
			if u.String() != before {
				t.Errorf("RedactURL mutated its input: %s", u)
			}
		})
	}
	if got := RedactURL(nil); got != "" {
		t.Errorf("RedactURL(nil) = %q", got)
	}
}

func TestRedactHeader(t *testing.T) {
	h := http.Header{
		"Authorization":       {"Bearer s3cret"},
		"Proxy-Authorization": {"Basic s3cret"},
		"Cookie":              {"session=s3cret"},
		"Set-Cookie":          {"session=s3cret; HttpOnly"},
		"X-Api-Key":           {"s3cret"},
		"X-Auth-Token":        {"s3cret"},
		"X-Client-Secret":     {"s3cret"},
		"Content-Type":        {"application/json"},
		"X-Request-Id":        {"abc"},
	}
	before := h.Clone()
	got := RedactHeader(h)
	for k, vs := range got {
		if strings.Contains(strings.Join(vs, ","), "s3cret") {
			t.Errorf("%s = %v leaks a secret", k, vs)
		}
	}
	if got.Get("Content-Type") != "application/json" || got.Get("X-Request-Id") != "abc" {
		t.Errorf("harmless headers changed: %v", got)
	}
	if h.Get("Authorization") != before.Get("Authorization") {
		t.Error("RedactHeader mutated its input")
	}
}

func TestRedactBody(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		truncated   bool
		want        string
	}{
		{name: "json members", contentType: "application/json",
			body: `{"user":"ann","password":"s3cret","nested":{"access_token":"s3cret","n":1},"list":[{"api_key":"s3cret"}]}`,
			want: `{"user":"ann","password":"REDACTED","nested":{"access_token":"REDACTED","n":1},"list":[{"api_key":"REDACTED"}]}`},
		{name: "secret object value", contentType: "application/json; charset=utf-8",
			body: `{"credentials":{"user":"a","pass":"s3cret"},"ok":true}`, want: `{"credentials":"REDACTED","ok":true}`},
		{name: "values named like secrets are kept", contentType: "application/json",
			body: `{"note":"token"}`, want: `{"note":"token"}`},
		{name: "problem+json", contentType: "application/problem+json",
			body: `{"title":"bad","token":"s3cret"}`, want: `{"title":"bad","token":"REDACTED"}`},
		{name: "cut mid-secret", contentType: "application/json",
			body: `{"a":1,"password":"s3c`, want: `{"a":1,"password":"…"}…(truncated)`},
		{name: "cut after secret", contentType: "application/json",
			body: `{"password":"s3cret","b":[1,2`, truncated: true, want: `{"password":"REDACTED","b":[1,2]}…(truncated)`},
		{name: "not json at all", contentType: "application/json", body: `s3cret`, want: `…(truncated)`},
		{name: "duplicate keys", contentType: "application/json",
			body: `{"token":"a","token":"s3cret"}`, want: `{"token":"REDACTED","token":"REDACTED"}`},
		{name: "form", contentType: "application/x-www-form-urlencoded",
			body: "grant_type=client_credentials&client_secret=s3cret", want: "client_secret=REDACTED&grant_type=client_credentials"},
		{name: "xml omitted", contentType: "application/xml", body: "<token>s3cret</token>", want: "<application/xml body omitted>"},
		{name: "plain text omitted", contentType: "text/plain", body: "s3cret", want: "<text/plain body omitted>"},
		{name: "no type omitted", body: "s3cret", want: "<untyped body omitted>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactBody(tt.contentType, []byte(tt.body), tt.truncated)
			if got != tt.want {
				t.Errorf("redactBody() =\n %s\nwant\n %s", got, tt.want)
			}
			if strings.Contains(got, "s3c") {
				t.Errorf("redactBody() leaks a secret: %s", got)
			}
		})
	}
}

func TestRoute(t *testing.T) {
	tests := []struct {
		path, want string
	}{
		{"/invoices/12345", "/invoices/{id}"},
		{"/v1/invoices", "/v1/invoices"},
		{"/users/0b8f3c1e-8e0a-4a8e-9c1f-2d7e5b6a9c10/orders/7", "/users/{id}/orders/{id}"},
		{"/files/a1b2c3d4e5f6a7b8c9d0", "/files/{id}"},
		{"/v2/items/latest", "/v2/items/latest"},
		{"", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.test"+tt.path+"?token=x", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := Route(req); got != tt.want {
				t.Errorf("Route() = %q, want %q", got, tt.want)
			}
		})
	}

	req, err := http.NewRequestWithContext(WithRoute(t.Context(), "/invoices/{invoiceID}"), http.MethodGet, "https://api.test/invoices/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := Route(req); got != "/invoices/{invoiceID}" {
		t.Errorf("Route() with WithRoute = %q", got)
	}
}
