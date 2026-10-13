// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package transport_test

import (
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/transport"
)

// trace records the order in which middleware see a request and response.
type trace struct {
	mu     sync.Mutex
	events []string
}

func (tr *trace) add(e string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.events = append(tr.events, e)
}

func (tr *trace) recording(name string) transport.Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		tr.add("build " + name)
		return transport.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			tr.add(name + " >")
			resp, err := next.RoundTrip(req)
			tr.add(name + " <")
			return resp, err
		})
	}
}

func TestChain(t *testing.T) {
	tests := []struct {
		name  string
		mws   func(*trace) []transport.Middleware
		trips int
		want  []string
	}{
		{
			name:  "no middleware is the base",
			mws:   func(*trace) []transport.Middleware { return nil },
			trips: 1,
			want:  []string{"base"},
		},
		{
			name: "first is outermost",
			mws: func(tr *trace) []transport.Middleware {
				return []transport.Middleware{tr.recording("a"), tr.recording("b"), tr.recording("c")}
			},
			trips: 1,
			want: []string{
				"build c", "build b", "build a",
				"a >", "b >", "c >", "base", "c <", "b <", "a <",
			},
		},
		{
			name: "nil middleware is skipped",
			mws: func(tr *trace) []transport.Middleware {
				return []transport.Middleware{tr.recording("a"), nil, tr.recording("b")}
			},
			trips: 1,
			want:  []string{"build b", "build a", "a >", "b >", "base", "b <", "a <"},
		},
		{
			name: "middleware is built once, not per request",
			mws: func(tr *trace) []transport.Middleware {
				return []transport.Middleware{tr.recording("a")}
			},
			trips: 2,
			want:  []string{"build a", "a >", "base", "a <", "a >", "base", "a <"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &trace{}
			base := &integrationtest.RecordingTransport{
				Respond: func(*http.Request) (*http.Response, error) {
					tr.add("base")
					return integrationtest.Response(http.StatusOK, ""), nil
				},
			}
			rt := transport.Chain(base, tt.mws(tr)...)
			for range tt.trips {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test/", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := rt.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			}
			if !slices.Equal(tr.events, tt.want) {
				t.Errorf("events:\n got %q\nwant %q", tr.events, tt.want)
			}
		})
	}
}
