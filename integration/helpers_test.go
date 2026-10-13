// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/integrationtest"
)

type invoice struct {
	ID     string  `json:"id"`
	Amount int     `json:"amount"`
	Note   *string `json:"note,omitzero"`
}

func jsonResponse(status int, body string) *http.Response {
	resp := integrationtest.Response(status, body)
	resp.Header.Set("Content-Type", "application/json")
	return resp
}

func TestGetJSON(t *testing.T) {
	var body *integrationtest.Body
	rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
		resp := jsonResponse(http.StatusOK, `{"id":"inv-1","amount":42}`)
		body = resp.Body.(*integrationtest.Body)
		return resp, nil
	}}
	c := newClient(t, "https://api.test/v1", rt)
	got, err := c.GetJSON[invoice](t.Context(), "/invoices/inv-1")
	if err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if got.ID != "inv-1" || got.Amount != 42 || got.Note != nil {
		t.Errorf("GetJSON() = %+v", got)
	}
	req := rt.Requests()[0]
	if req.Method != http.MethodGet || req.URL.String() != "https://api.test/v1/invoices/inv-1" {
		t.Errorf("request = %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	if !body.Closed() {
		t.Error("response body not closed")
	}
}

func TestSendJSON(t *testing.T) {
	tests := []struct {
		name   string
		method string
		send   func(*integration.Client, *testing.T, invoice) (invoice, error)
	}{
		{name: "post", method: http.MethodPost, send: func(c *integration.Client, t *testing.T, in invoice) (invoice, error) {
			return c.PostJSON[invoice, invoice](t.Context(), "/invoices", in)
		}},
		{name: "put", method: http.MethodPut, send: func(c *integration.Client, t *testing.T, in invoice) (invoice, error) {
			return c.PutJSON[invoice, invoice](t.Context(), "/invoices", in)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{Respond: func(req *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				return jsonResponse(http.StatusCreated, string(b)), nil // echo
			}}
			c := newClient(t, "https://api.test", rt)
			in := invoice{ID: "inv-1", Amount: 7, Note: new("rush")}
			got, err := tt.send(c, t, in)
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if got.ID != in.ID || got.Amount != in.Amount || got.Note == nil || *got.Note != "rush" {
				t.Errorf("response = %+v", got)
			}

			req := rt.Requests()[0]
			if req.Method != tt.method {
				t.Errorf("method = %s, want %s", req.Method, tt.method)
			}
			if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json" {
				t.Errorf("headers = %v", req.Header)
			}
			const wantBody = `{"id":"inv-1","amount":7,"note":"rush"}`
			if req.ContentLength != int64(len(wantBody)) {
				t.Errorf("ContentLength = %d, want %d", req.ContentLength, len(wantBody))
			}
			if req.GetBody == nil {
				t.Fatal("GetBody is nil; the request cannot be retried")
			}
			for range 2 { // replayable more than once
				rc, err := req.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(rc)
				if string(b) != wantBody {
					t.Errorf("GetBody() = %s, want %s", b, wantBody)
				}
			}
		})
	}
}

func TestPostJSON_EncodeErrorSendsNothing(t *testing.T) {
	rt := &integrationtest.RecordingTransport{}
	_, err := newClient(t, "https://api.test", rt).PostJSON[chan int, struct{}](t.Context(), "/x", make(chan int))
	if err == nil {
		t.Fatal("PostJSON() error = nil")
	}
	if n := len(rt.Requests()); n != 0 {
		t.Errorf("sent %d requests, want 0", n)
	}
}

func TestJSON_EmptyResponse(t *testing.T) {
	tests := []struct {
		name string
		resp func() *http.Response
	}{
		{name: "204", resp: func() *http.Response { return integrationtest.Response(http.StatusNoContent, "") }},
		{name: "content-length 0", resp: func() *http.Response { return jsonResponse(http.StatusOK, "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
				return tt.resp(), nil
			}}
			got, err := newClient(t, "https://api.test", rt).PostJSON[invoice, invoice](t.Context(), "/x", invoice{})
			if err != nil || got != (invoice{}) {
				t.Errorf("PostJSON() = %+v, %v; want zero, nil", got, err)
			}
		})
	}
}

func TestGetJSON_Errors(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		status     int
		opts       []integration.Option
		wantStatus int
		wantIs     error
		wantOK     bool
	}{
		{name: "duplicate key rejected by default", status: 200, body: `{"id":"a","id":"b"}`,
			wantStatus: 200, wantIs: jsontext.ErrDuplicateName},
		{name: "duplicate key allowed per client", status: 200, body: `{"id":"a","id":"b"}`,
			// This upstream is known to repeat keys; take the last one.
			opts: []integration.Option{integration.WithJSONOptions(jsontext.AllowDuplicateNames(true))}, wantOK: true},
		{name: "malformed body", status: 200, body: `{"id":`, wantStatus: 200},
		{name: "wrong type", status: 200, body: `{"amount":"many"}`, wantStatus: 200},
		{name: "status error", status: 404, body: `{"error":"missing"}`, wantStatus: 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body *integrationtest.Body
			rt := &integrationtest.RecordingTransport{Respond: func(*http.Request) (*http.Response, error) {
				resp := jsonResponse(tt.status, tt.body)
				body = resp.Body.(*integrationtest.Body)
				return resp, nil
			}}
			got, err := newClient(t, "https://api.test", rt, tt.opts...).GetJSON[invoice](t.Context(), "/x")
			if !body.Closed() {
				t.Error("response body not closed")
			}
			if tt.wantOK {
				if err != nil || got.ID != "b" {
					t.Errorf("GetJSON() = %+v, %v", got, err)
				}
				return
			}
			if got != (invoice{}) {
				t.Errorf("GetJSON() = %+v on error, want zero", got)
			}
			e, ok := errors.AsType[*integration.Error](err)
			if !ok {
				t.Fatalf("error = %v, want *integration.Error", err)
			}
			if e.StatusCode != tt.wantStatus || e.Retryable {
				t.Errorf("Error = %+v", e)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.wantIs)
			}
		})
	}
}

func TestPostJSON_TestServer(t *testing.T) {
	srv := integrationtest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/json" || string(b) != `{"id":"in","amount":9}` {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"srv","amount":9}`)
	}))

	c := newClient(t, srv.URL, srv.Client().Transport)
	got, err := c.PostJSON[invoice, invoice](t.Context(), "/invoices", invoice{ID: "in", Amount: 9})
	if err != nil {
		t.Fatalf("PostJSON() error = %v", err)
	}
	if got.ID != "srv" || got.Amount != 9 {
		t.Errorf("PostJSON() = %+v", got)
	}
}
