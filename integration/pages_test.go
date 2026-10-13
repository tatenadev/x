// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/integrationtest"
)

// pagedAPI serves pages keyed by the "page" query value. Each page has a
// body and an optional Link header.
type pagedAPI map[string]struct {
	status int
	body   string
	link   string
}

func (p pagedAPI) transport() *integrationtest.RecordingTransport {
	return &integrationtest.RecordingTransport{Respond: func(req *http.Request) (*http.Response, error) {
		pg, ok := p[req.URL.Query().Get("page")]
		if !ok {
			return integrationtest.Response(http.StatusNotFound, ""), nil
		}
		status := pg.status
		if status == 0 {
			status = http.StatusOK
		}
		resp := jsonResponse(status, pg.body)
		if pg.link != "" {
			resp.Header.Set("Link", pg.link)
		}
		return resp, nil
	}}
}

// threePages links page 1 -> 2 (relative) -> 3 (absolute).
var threePages = pagedAPI{
	"1": {body: `[1, 2]`, link: `</v1/items?page=2>; rel="next", </v1/items?page=3>; rel="last"`},
	"2": {body: `[3]`, link: `<https://api.test/v1/items?page=3>; rel="next"`},
	"3": {body: `[4, 5]`},
}

type yielded struct {
	v   int
	err error
}

func collect(seq func(func(int, error) bool), stopAfter int) []yielded {
	var out []yielded
	for v, err := range seq {
		out = append(out, yielded{v, err})
		if len(out) == stopAfter {
			break
		}
	}
	return out
}

func values(ys []yielded) []int {
	var vs []int
	for _, y := range ys {
		if y.err == nil {
			vs = append(vs, y.v)
		}
	}
	return vs
}

func TestPages(t *testing.T) {
	tests := []struct {
		name      string
		api       pagedAPI
		next      integration.PageFunc
		stopAfter int
		want      []int
		wantErr   bool
		wantReqs  int
	}{
		{name: "all pages", api: threePages, next: integration.LinkHeaderNext, want: []int{1, 2, 3, 4, 5}, wantReqs: 3},
		{name: "break mid-page", api: threePages, next: integration.LinkHeaderNext, stopAfter: 1, want: []int{1}, wantReqs: 1},
		{name: "break at end of page", api: threePages, next: integration.LinkHeaderNext, stopAfter: 2, want: []int{1, 2}, wantReqs: 1},
		{name: "break on next page", api: threePages, next: integration.LinkHeaderNext, stopAfter: 3, want: []int{1, 2, 3}, wantReqs: 2},
		{name: "nil next is one page", api: threePages, want: []int{1, 2}, wantReqs: 1},
		{
			name: "error on second page",
			api: pagedAPI{
				"1": {body: `[1, 2]`, link: `<?page=2>; rel="next"`},
				"2": {status: http.StatusServiceUnavailable},
			},
			next: integration.LinkHeaderNext, want: []int{1, 2}, wantErr: true, wantReqs: 2,
		},
		{
			name: "decode error mid-page",
			api:  pagedAPI{"1": {body: `[1, "two", 3]`, link: `<?page=2>; rel="next"`}},
			next: integration.LinkHeaderNext, want: []int{1}, wantErr: true, wantReqs: 1,
		},
		{
			name: "next func error",
			api:  threePages,
			next: func(*http.Response) (string, error) { return "", errors.New("no cursor") },
			want: []int{1, 2}, wantErr: true, wantReqs: 1,
		},
		{
			name: "next repeats the page",
			api:  pagedAPI{"1": {body: `[1]`, link: `</v1/items?page=1>; rel="next"`}},
			next: integration.LinkHeaderNext, want: []int{1}, wantErr: true, wantReqs: 1,
		},
		{
			name: "next leaves the origin",
			api:  pagedAPI{"1": {body: `[1]`, link: `<https://evil.test/v1/items?page=2>; rel="next"`}},
			next: integration.LinkHeaderNext, want: []int{1}, wantErr: true, wantReqs: 1,
		},
		{
			name: "null and 204 pages are empty",
			api: pagedAPI{
				"1": {body: `null`, link: `<?page=2>; rel="next"`},
				"2": {status: http.StatusNoContent},
			},
			next: integration.LinkHeaderNext, wantReqs: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := tt.api.transport()
			c := newClient(t, "https://api.test/v1", rt)
			got := collect(c.Pages[int](t.Context(), "/items?page=1", tt.next), tt.stopAfter)

			if vs := values(got); !slices.Equal(vs, tt.want) {
				t.Errorf("values = %v, want %v", vs, tt.want)
			}
			var errs int
			for i, y := range got {
				if y.err != nil {
					errs++
					if i != len(got)-1 {
						t.Errorf("error %v was not the last item", y.err)
					}
					if y.v != 0 {
						t.Errorf("error yielded with value %d, want zero", y.v)
					}
				}
			}
			if wantErrs := map[bool]int{false: 0, true: 1}[tt.wantErr]; errs != wantErrs {
				t.Errorf("got %d errors, want %d: %v", errs, wantErrs, got)
			}
			reqs := rt.Requests()
			if len(reqs) != tt.wantReqs {
				t.Errorf("made %d requests, want %d", len(reqs), tt.wantReqs)
			}
			for _, r := range reqs {
				if r.URL.Host != "api.test" || r.Header.Get("Accept") != "application/json" {
					t.Errorf("request %s Accept=%q", r.URL, r.Header.Get("Accept"))
				}
			}
		})
	}
}

func TestPages_ErrorIsTyped(t *testing.T) {
	api := pagedAPI{"1": {status: http.StatusTooManyRequests}}
	c := newClient(t, "https://api.test/v1", api.transport())
	for _, err := range c.Pages[int](t.Context(), "/items?page=1", nil) {
		e, ok := errors.AsType[*integration.Error](err)
		if !ok || e.StatusCode != http.StatusTooManyRequests || !e.Retryable {
			t.Errorf("error = %v, want retryable *integration.Error 429", err)
		}
	}
}

func TestPages_Restartable(t *testing.T) {
	rt := threePages.transport()
	seq := newClient(t, "https://api.test/v1", rt).Pages[int](t.Context(), "/items?page=1", integration.LinkHeaderNext)
	for range 2 {
		if got := values(collect(seq, 0)); !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
			t.Errorf("values = %v", got)
		}
	}
	if n := len(rt.Requests()); n != 6 {
		t.Errorf("made %d requests, want 6", n)
	}
}

func TestLinkHeaderNext(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
		want    string
		wantErr bool
	}{
		{name: "none"},
		{name: "single", headers: []string{`<https://a.test/?p=2>; rel="next"`}, want: "https://a.test/?p=2"},
		{name: "unquoted rel", headers: []string{`</p2>; rel=next`}, want: "/p2"},
		{name: "among others", headers: []string{`</first>; rel="first", </p2>; rel="next", </last>; rel="last"`}, want: "/p2"},
		{name: "rel list", headers: []string{`</p2>; rel="next last"`}, want: "/p2"},
		{name: "case-insensitive", headers: []string{`</p2>; REL="Next"`}, want: "/p2"},
		{name: "comma in quoted param", headers: []string{`</a>; title="x, y"; rel="prev", </p2>; rel="next"`}, want: "/p2"},
		{name: "second header", headers: []string{`</prev>; rel="prev"`, `</p2>; rel="next"`}, want: "/p2"},
		{name: "no next", headers: []string{`</prev>; rel="prev"`}},
		{name: "nextish is not next", headers: []string{`</p2>; rel="next-archive"`}},
		{name: "malformed", headers: []string{`https://a.test/; rel="next"`}, wantErr: true},
		{name: "unterminated", headers: []string{`<https://a.test/; rel="next"`}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			for _, h := range tt.headers {
				resp.Header.Add("Link", h)
			}
			got, err := integration.LinkHeaderNext(resp)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("LinkHeaderNext() = %q, %v; want %q, err=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func ExampleClient_Pages() {
	api := pagedAPI{
		"1": {body: `[1, 2]`, link: `<?page=2>; rel="next"`},
		"2": {body: `[3]`},
	}
	c, err := integration.New("numbers", "https://api.test/v1", integration.WithBaseTransport(api.transport()))
	if err != nil {
		panic(err)
	}
	for n, err := range c.Pages[int](context.Background(), "/items?page=1", integration.LinkHeaderNext) {
		if err != nil {
			fmt.Println("error:", err)
			break
		}
		fmt.Println(n)
	}
	// Output:
	// 1
	// 2
	// 3
}
