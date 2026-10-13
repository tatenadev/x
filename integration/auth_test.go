// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/transport"
)

func TestWithTokenSource_AuthIsInnermost(t *testing.T) {
	var seenByCaller string
	callerMW := func(next http.RoundTripper) http.RoundTripper {
		return transport.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			seenByCaller = req.Header.Get("Authorization")
			return next.RoundTrip(req)
		})
	}
	rt := &integrationtest.RecordingTransport{}
	c := newClient(t, "https://api.test", rt,
		integration.WithMiddleware(callerMW),
		integration.WithTokenSource(auth.StaticToken("s3cret")))
	if err := c.Delete(t.Context(), "/x"); err != nil {
		t.Fatal(err)
	}
	if got := rt.Requests()[0].Header.Get("Authorization"); got != "Bearer s3cret" {
		t.Errorf("Authorization = %q", got)
	}
	if seenByCaller != "" {
		t.Errorf("caller middleware saw Authorization %q; auth must run inside it", seenByCaller)
	}
}

func TestWithTokenSource_Nil(t *testing.T) {
	_, err := integration.New("s", "https://api.test", integration.WithTokenSource(nil))
	if !errors.Is(err, integration.ErrInvalidConfig) {
		t.Errorf("New() error = %v, want ErrInvalidConfig", err)
	}
}

func TestClientCredentials_EndToEnd(t *testing.T) {
	idp := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"from-idp","token_type":"Bearer","expires_in":3600}`)
	}))
	defer idp.Close()
	api := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer from-idp" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"inv-1","amount":3}`)
	}))
	defer api.Close()

	cc, err := auth.NewClientCredentials(idp.URL+"/token", "id", "secret", auth.WithDoer(idp.Client()))
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, api.URL, api.Client().Transport, integration.WithTokenSource(cc))
	got, err := c.GetJSON[invoice](t.Context(), "/invoices/inv-1")
	if err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if got.ID != "inv-1" {
		t.Errorf("GetJSON() = %+v", got)
	}
}
