// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package codec_test

import (
	"bytes"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tatenadev/x/integration/codec"
)

type Paging struct {
	Page int `form:"page,omitempty"`
}

type tokenRequest struct {
	GrantType string    `form:"grant_type"`
	Scope     []string  `form:"scope,omitempty"`
	Audience  string    `form:"audience,omitempty"`
	Count     int       `form:"count"`
	Big       uint64    `form:"big,omitempty"`
	Ratio     float64   `form:"ratio,omitempty"`
	Active    bool      `form:"active"`
	Strict    *bool     `form:"strict"`
	Limit     *int      `form:"limit,omitempty"`
	Since     time.Time `form:"since,omitempty"`
	Raw       []byte    `form:"raw,omitempty"`
	Secret    string    `form:"-"`
	Untagged  string
	Paging
	unexported string
}

func TestForm_Encode(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "zero struct keeps non-omitempty fields",
			in:   tokenRequest{},
			want: "Untagged=&active=false&count=0&grant_type=",
		},
		{
			name: "full struct",
			in: &tokenRequest{
				GrantType: "client_credentials", Scope: []string{"read", "write"}, Count: 2, Big: 7, Ratio: 0.5,
				Active: true, Strict: new(false), Limit: new(10), Since: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
				Raw: []byte("hi"), Secret: "s", Untagged: "u", Page: 3, unexported: "x",
			},
			want: "Untagged=u&active=true&big=7&count=2&grant_type=client_credentials&limit=10&page=3&ratio=0.5" +
				"&raw=hi&scope=read&scope=write&since=2026-01-02T00%3A00%3A00Z&strict=false",
		},
		{name: "url.Values", in: url.Values{"b": {"2"}, "a": {"1", "x y"}}, want: "a=1&a=x+y&b=2"},
		{name: "map[string]string", in: map[string]string{"k": "v&w"}, want: "k=v%26w"},
		{name: "map[string][]string", in: map[string][]string{"k": {"1", "2"}}, want: "k=1&k=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := (codec.Form{}).Encode(&buf, tt.in); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tt.want {
				t.Errorf("Encode =\n %s\nwant\n %s", buf.String(), tt.want)
			}
		})
	}
}

func TestForm_EncodeUnsupported(t *testing.T) {
	tests := []any{
		struct {
			M map[string]int `form:"m"`
		}{M: map[string]int{}},
		42,
		(*tokenRequest)(nil),
	}
	for _, in := range tests {
		if err := (codec.Form{}).Encode(&bytes.Buffer{}, in); err == nil {
			t.Errorf("Encode(%T) error = nil", in)
		}
	}
}

func TestForm_DecodeRoundTrip(t *testing.T) {
	in := tokenRequest{
		GrantType: "client_credentials", Scope: []string{"read", "write"}, Count: -2, Big: 7, Ratio: 0.25,
		Active: true, Strict: new(true), Limit: new(10), Since: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Raw: []byte("hi"), Untagged: "u", Page: 3,
	}
	var buf bytes.Buffer
	if err := (codec.Form{}).Encode(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out tokenRequest
	if err := (codec.Form{}).Decode(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round trip =\n %+v\nwant\n %+v", out, in)
	}
}

func TestForm_DecodeValues(t *testing.T) {
	var got url.Values
	if err := (codec.Form{}).Decode(strings.NewReader("access_token=abc&expires_in=3600"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Get("access_token") != "abc" || got.Get("expires_in") != "3600" {
		t.Errorf("Decode = %v", got)
	}
}

func TestForm_DecodeErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		into any
	}{
		{name: "bad int", in: "count=x", into: &tokenRequest{}},
		{name: "bad bool", in: "active=maybe", into: &tokenRequest{}},
		{name: "bad time", in: "since=yesterday", into: &tokenRequest{}},
		{name: "not a pointer", in: "a=1", into: tokenRequest{}},
		{name: "bad escape", in: "a=%zz", into: &url.Values{}},
		{name: "too large", in: "a=" + strings.Repeat("x", 1<<20), into: &url.Values{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := (codec.Form{}).Decode(strings.NewReader(tt.in), tt.into); err == nil {
				t.Error("Decode() error = nil")
			}
		})
	}
}

func TestForContentType(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" = not found
	}{
		{"application/json", codec.ContentTypeJSON},
		{"application/json; charset=utf-8", codec.ContentTypeJSON},
		{"application/problem+json", codec.ContentTypeJSON},
		{"APPLICATION/JSON", codec.ContentTypeJSON},
		{"application/xml", codec.ContentTypeXML},
		{"text/xml; charset=utf-8", codec.ContentTypeXML},
		{"application/atom+xml", codec.ContentTypeXML},
		{"application/x-www-form-urlencoded", codec.ContentTypeForm},
		{"text/html", ""},
		{"", ""},
		{";;", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c, ok := codec.ForContentType(tt.in)
			got := ""
			if ok {
				got = c.ContentType()
			}
			if got != tt.want {
				t.Errorf("ForContentType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestXML_RoundTrip(t *testing.T) {
	type item struct {
		ID   string `xml:"id,attr"`
		Name string `xml:"name"`
	}
	var buf bytes.Buffer
	if err := (codec.XML{}).Encode(&buf, item{ID: "1", Name: "a&b"}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), `<item id="1"><name>a&amp;b</name></item>`; got != want {
		t.Errorf("Encode = %s, want %s", got, want)
	}
	var got item
	if err := (codec.XML{}).Decode(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got != (item{ID: "1", Name: "a&b"}) {
		t.Errorf("Decode = %+v", got)
	}
}
