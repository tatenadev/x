// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package codec_test

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tatenadev/x/integration/codec"
)

type invoice struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

func TestJSON_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := (codec.JSON{}).Encode(&buf, invoice{ID: "a", Amount: 5}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), `{"id":"a","amount":5}`; got != want {
		t.Errorf("Encode = %s, want %s", got, want)
	}
	var got invoice
	if err := (codec.JSON{}).Decode(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got != (invoice{ID: "a", Amount: 5}) {
		t.Errorf("Decode = %+v", got)
	}
}

func TestJSON_StrictDefaults(t *testing.T) {
	tests := []struct {
		name    string
		codec   codec.JSON
		in      string
		wantErr error // nil: must succeed; errAny: any error
		want    invoice
	}{
		{name: "duplicate key rejected", codec: codec.JSON{}, in: `{"id":"a","id":"b"}`, wantErr: jsontext.ErrDuplicateName},
		{name: "duplicate key allowed when relaxed", codec: codec.NewJSON(jsontext.AllowDuplicateNames(true)),
			in: `{"id":"a","id":"b"}`, want: invoice{ID: "b"}},
		{name: "invalid UTF-8 rejected", codec: codec.JSON{}, in: "{\"id\":\"\xff\"}", wantErr: errAny},
		{name: "trailing data rejected", codec: codec.JSON{}, in: `{"id":"a"} {}`, wantErr: errAny},
		// v1 matched names case-insensitively; v2 does not. Pinned for migrations.
		{name: "names match case-sensitively", codec: codec.JSON{}, in: `{"ID":"a","amount":1}`, want: invoice{Amount: 1}},
		{name: "unknown members ignored", codec: codec.JSON{}, in: `{"id":"a","extra":true}`, want: invoice{ID: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got invoice
			err := tt.codec.Decode(strings.NewReader(tt.in), &got)
			switch {
			case tt.wantErr == nil && err != nil:
				t.Fatalf("Decode() error = %v", err)
			case tt.wantErr == errAny && err == nil, tt.wantErr != nil && tt.wantErr != errAny && !errors.Is(err, tt.wantErr):
				t.Fatalf("Decode() error = %v, want %v", err, tt.wantErr)
			case tt.wantErr == nil && got != tt.want:
				t.Errorf("Decode() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

var errAny = errors.New("any error")

// listQuery is a request DTO with optional fields.
type listQuery struct {
	Limit  *int      `json:"limit,omitzero"` // pointer: 0 is a valid limit
	Cursor string    `json:"cursor,omitzero"`
	Since  time.Time `json:"since,omitzero"`
	Tags   []string  `json:"tags,omitzero"`
}

func TestJSON_OptionalFields(t *testing.T) {
	tests := []struct {
		name string
		in   listQuery
		want string
	}{
		{name: "all unset", in: listQuery{}, want: `{}`},
		{name: "limit set", in: listQuery{Limit: new(50)}, want: `{"limit":50}`},
		{name: "explicit zero limit kept", in: listQuery{Limit: new(0)}, want: `{"limit":0}`},
		{name: "empty but non-nil slice kept", in: listQuery{Tags: []string{}}, want: `{"tags":[]}`},
		{name: "time set", in: listQuery{Since: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
			want: `{"since":"2026-01-02T00:00:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := (codec.JSON{}).Encode(&buf, tt.in); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tt.want {
				t.Errorf("Encode = %s, want %s", buf.String(), tt.want)
			}
		})
	}
}

func TestJSON_Each(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []int
		wantErr bool
	}{
		{name: "array", in: `[1, 2, 3]`, want: []int{1, 2, 3}},
		{name: "empty array", in: `[]`},
		{name: "null", in: `null`},
		{name: "whitespace around", in: " \n[1]\n ", want: []int{1}},
		{name: "object", in: `{"a":1}`, wantErr: true},
		{name: "scalar", in: `1`, wantErr: true},
		{name: "empty input", in: ``, wantErr: true},
		{name: "truncated", in: `[1, 2`, want: []int{1, 2}, wantErr: true},
		{name: "bad element", in: `[1, "x", 3]`, want: []int{1}, wantErr: true},
		{name: "trailing value", in: `[1] [2]`, want: []int{1}, wantErr: true},
		{name: "trailing after null", in: `null 1`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []int
			var gotErr error
			n := 0
			for v, err := range (codec.JSON{}).Each[int](strings.NewReader(tt.in)) {
				n++
				if err != nil {
					if gotErr != nil {
						t.Fatal("yielded a second error")
					}
					gotErr = err
					continue
				}
				if gotErr != nil {
					t.Fatal("yielded a value after an error")
				}
				got = append(got, v)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("values = %v, want %v", got, tt.want)
			}
			if (gotErr != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", gotErr, tt.wantErr)
			}
		})
	}
}

func TestJSON_EachStopsEarly(t *testing.T) {
	// Everything after the first element is invalid; stopping after it
	// must not decode (or report) the rest.
	for v, err := range (codec.JSON{}).Each[int](strings.NewReader(`[1, !!!`)) {
		if err != nil {
			t.Fatalf("error = %v after the consumer stopped", err)
		}
		if v != 1 {
			t.Fatalf("v = %d", v)
		}
		break
	}
}

func TestJSON_EachStrict(t *testing.T) {
	var gotErr error
	for _, err := range (codec.JSON{}).Each[invoice](strings.NewReader(`[{"id":"a","id":"b"}]`)) {
		gotErr = err
	}
	if !errors.Is(gotErr, jsontext.ErrDuplicateName) {
		t.Errorf("error = %v, want ErrDuplicateName", gotErr)
	}
}
