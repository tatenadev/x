// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package google

import (
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/tatenadev/x/google/gsheets"
	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/integrationtest"
)

// The golden payloads in testdata follow the documented Sheets API v4
// response shapes (spreadsheets.get, spreadsheets.values.append, and the
// standard error envelope). They are representative, not captured from
// live traffic; they pin that the new transport hands Google's own JSON
// decoding the bytes unchanged.
const spreadsheetID = "1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgvE2upms"

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeSheets serves scripted responses and records what it was sent.
type fakeSheets struct {
	mu    sync.Mutex
	steps []func(w http.ResponseWriter)
	seen  []*http.Request
}

func (f *fakeSheets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	i := min(len(f.seen), len(f.steps)-1)
	f.seen = append(f.seen, r.Clone(r.Context()))
	f.mu.Unlock()
	f.steps[i](w)
}

func (f *fakeSheets) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

func reply(status int, body []byte) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(status)
		w.Write(body)
	}
}

func newSheets(t *testing.T, f *fakeSheets) *sheets.Service {
	t.Helper()
	srv := integrationtest.Server(t, f)
	hc, err := newClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.test", TokenType: "Bearer"}),
		integration.WithBaseTransport(srv.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := sheets.NewService(t.Context(), option.WithHTTPClient(hc), option.WithEndpoint(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestClient_GoldenSpreadsheet(t *testing.T) {
	f := &fakeSheets{steps: []func(http.ResponseWriter){reply(200, golden(t, "sheets_get.golden.json"))}}
	ss, err := newSheets(t, f).Spreadsheets.Get(spreadsheetID).Context(t.Context()).Do()
	if err != nil {
		t.Fatalf("spreadsheets.get: %v", err)
	}
	if ss.Properties.Title != "Budget 2026" || len(ss.Sheets) != 2 ||
		ss.Sheets[1].Properties.Title != "Transactions" || ss.Sheets[1].Properties.SheetId != 1873441652 ||
		ss.Sheets[0].Properties.GridProperties.FrozenRowCount != 1 {
		t.Errorf("decoded spreadsheet = %+v", ss)
	}

	req := f.requests()[0]
	if req.Header.Get("Authorization") != "Bearer ya29.test" {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-ID: the request did not go through the integration layer")
	}
}

func TestClient_GoldenAppendThroughGsheets(t *testing.T) {
	f := &fakeSheets{steps: []func(http.ResponseWriter){reply(200, golden(t, "values_append.golden.json"))}}
	if err := gsheets.AppendRows(t.Context(), newSheets(t, f), spreadsheetID, [][]any{{"a", 1, true}, {"b", 2, false}}); err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	req := f.requests()[0]
	if req.Method != http.MethodPost || req.URL.Query().Get("valueInputOption") != "USER_ENTERED" {
		t.Errorf("request = %s %s", req.Method, req.URL)
	}
}

func TestClient_RetryBehavior(t *testing.T) {
	tests := []struct {
		name     string
		call     func(*sheets.Service, *testing.T) error
		first    func(http.ResponseWriter)
		then     func(http.ResponseWriter)
		attempts int
		wantErr  int // googleapi.Error code, 0 = success
	}{
		{
			name: "GET retried after 500 backendError",
			call: func(s *sheets.Service, t *testing.T) error {
				_, err := s.Spreadsheets.Get(spreadsheetID).Context(t.Context()).Do()
				return err
			},
			first: reply(500, golden(t, "error_backend.golden.json")), then: reply(200, golden(t, "sheets_get.golden.json")),
			attempts: 2,
		},
		{
			name: "GET retried after 503",
			call: func(s *sheets.Service, t *testing.T) error {
				_, err := s.Spreadsheets.Get(spreadsheetID).Context(t.Context()).Do()
				return err
			},
			first: reply(503, nil), then: reply(200, golden(t, "sheets_get.golden.json")),
			attempts: 2,
		},
		{
			// Behavior change: the old client retried POST on 429/503.
			name: "append (POST) not retried without an idempotency key",
			call: func(s *sheets.Service, t *testing.T) error {
				return gsheets.AppendRows(t.Context(), s, spreadsheetID, [][]any{{"a"}})
			},
			first: reply(503, nil), then: reply(200, golden(t, "values_append.golden.json")),
			attempts: 1, wantErr: 503,
		},
		{
			// The oauth2 token source cannot be invalidated, so a 401
			// retry would only resend the same token.
			name: "401 not retried",
			call: func(s *sheets.Service, t *testing.T) error {
				_, err := s.Spreadsheets.Get(spreadsheetID).Context(t.Context()).Do()
				return err
			},
			first: reply(401, nil), then: reply(200, golden(t, "sheets_get.golden.json")),
			attempts: 1, wantErr: 401,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeSheets{steps: []func(http.ResponseWriter){tt.first, tt.then}}
				err := tt.call(newSheets(t, f), t)
				if n := len(f.requests()); n != tt.attempts {
					t.Errorf("attempts = %d, want %d", n, tt.attempts)
				}
				if tt.wantErr == 0 {
					if err != nil {
						t.Fatalf("error = %v", err)
					}
					return
				}
				if ge, ok := errors.AsType[*googleapi.Error](err); !ok || ge.Code != tt.wantErr {
					t.Errorf("error = %v, want googleapi.Error %d", err, tt.wantErr)
				}
			})
		})
	}
}

func TestClient_Timeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		hc, err := newClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"}),
			integration.WithBaseTransport(srv.Client().Transport))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/slow", nil)
		if _, err := hc.Do(req); err == nil {
			t.Fatal("hung request succeeded")
		}
		if elapsed := time.Since(start); elapsed != clientTimeout {
			t.Errorf("gave up after %v, want %v", elapsed, clientTimeout)
		}
	})
}

func TestOAuth2Source(t *testing.T) {
	tests := []struct {
		name    string
		tok     *oauth2.Token
		wantErr bool
	}{
		{name: "empty type is bearer", tok: &oauth2.Token{AccessToken: "a"}},
		{name: "lowercase bearer", tok: &oauth2.Token{AccessToken: "a", TokenType: "bearer"}},
		{name: "mac rejected", tok: &oauth2.Token{AccessToken: "a", TokenType: "MAC"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := oauth2Source{oauth2.StaticTokenSource(tt.tok)}.Token(t.Context())
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v", err)
			}
			if err == nil && (tok.Type != "Bearer" || tok.Value != "a") {
				t.Errorf("token = %#v", tok)
			}
		})
	}
}
