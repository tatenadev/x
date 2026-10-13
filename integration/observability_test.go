// Copyright (c) 2026 Paweł Zaremba
// SPDX-License-Identifier: MIT

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tatenadev/x/integration"
	"github.com/tatenadev/x/integration/auth"
	"github.com/tatenadev/x/integration/integrationtest"
	"github.com/tatenadev/x/integration/transport"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type recorder struct {
	mu   sync.Mutex
	recs []transport.RequestInfo
}

func (r *recorder) RecordRequest(_ context.Context, info transport.RequestInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, info)
}

type headerTracer struct {
	mu    sync.Mutex
	spans int
}

func (h *headerTracer) Start(ctx context.Context, _ *http.Request) (context.Context, func(*http.Response, error)) {
	h.mu.Lock()
	h.spans++
	h.mu.Unlock()
	return ctx, func(*http.Response, error) {}
}

func (h *headerTracer) Inject(_ context.Context, hdr http.Header) {
	hdr.Set("Traceparent", "00-trace-span-01")
}

func TestClient_Observability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer
		rec := &recorder{}
		tr := &headerTracer{}
		seq := &statusSequence{statuses: []int{503, 204}}
		srv := integrationtest.Server(t, seq)
		c := newClient(t, srv.URL, srv.Client().Transport,
			integration.WithRetry(3, time.Millisecond, time.Millisecond),
			integration.WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
			integration.WithMetrics(rec),
			integration.WithTracer(tr),
			integration.WithTokenSource(auth.StaticToken("s3cret")))

		ctx := integration.WithRoute(t.Context(), "/invoices/{id}")
		if err := c.Delete(ctx, "/invoices/42?access_token=s3cret"); err != nil {
			t.Fatal(err)
		}

		seen := seq.seen()
		if len(seen) != 2 {
			t.Fatalf("server saw %d requests", len(seen))
		}
		id := seen[0].Get("X-Request-Id")
		for i, h := range seen {
			if h.Get("Traceparent") == "" || h.Get("X-Request-Id") != id {
				t.Errorf("attempt %d headers = %v", i+1, h)
			}
		}
		if tr.spans != 1 {
			t.Errorf("%d spans, want 1", tr.spans)
		}
		if len(rec.recs) != 1 || rec.recs[0].Route != "/invoices/{id}" || rec.recs[0].Attempts != 2 || rec.recs[0].StatusCode != 204 {
			t.Errorf("metrics = %+v", rec.recs)
		}

		out := logs.String()
		if strings.Contains(out, "s3cret") {
			t.Errorf("logs leak a secret:\n%s", out)
		}
		if n := strings.Count(out, `"msg":"integration call"`); n != 1 {
			t.Errorf("%d call lines, want 1:\n%s", n, out)
		}
		if n := strings.Count(out, `"msg":"integration attempt"`); n != 2 {
			t.Errorf("%d attempt lines, want 2", n)
		}
		if !strings.Contains(out, `"request_id":"`+id+`"`) || !strings.Contains(out, `"attempts":2`) {
			t.Errorf("call line lacks request ID or attempts:\n%s", out)
		}
	})
}

func TestClient_BreakerTransitionsLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer
		rt := &integrationtest.RecordingTransport{Respond: integrationtest.Script(
			integrationtest.Status(503, ""), integrationtest.Status(204, ""))}
		c := newClient(t, "https://api.test", rt, integration.WithBreaker(1, time.Second),
			integration.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
		c.Delete(t.Context(), "/x")
		time.Sleep(time.Second)
		if err := c.Delete(t.Context(), "/x"); err != nil {
			t.Fatal(err)
		}
		out := logs.String()
		for _, want := range []string{
			`"level":"WARN","msg":"integration circuit breaker","service":"billing","from":"closed","to":"open"`,
			`"from":"open","to":"half-open"`,
			`"from":"half-open","to":"closed"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %s in:\n%s", want, out)
			}
		}
	})
}

func TestNew_InvalidObservabilityOptions(t *testing.T) {
	for i, opt := range []integration.Option{integration.WithLogger(nil), integration.WithMetrics(nil), integration.WithTracer(nil)} {
		if _, err := integration.New("s", "https://api.test", opt); !errors.Is(err, integration.ErrInvalidConfig) {
			t.Errorf("option %d: New() error = %v, want ErrInvalidConfig", i, err)
		}
	}
}

// TestCoreImportsStdlibOnly pins the dependency policy: every non-test file
// under integration/ imports only the standard library and the layer's own
// packages. The OpenTelemetry adapter is the one listed exception, and it
// may import only OpenTelemetry.
func TestCoreImportsStdlibOnly(t *testing.T) {
	const self = "github.com/tatenadev/x/integration"
	allowed := map[string]string{"otelx": "go.opentelemetry.io/otel"} // package dir -> allowed module prefix
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(p, "/")
			prefix, ok := allowed[filepath.Dir(path)]
			if strings.Contains(first, ".") && !strings.HasPrefix(p, self) && !(ok && strings.HasPrefix(p, prefix)) {
				t.Errorf("%s imports %s", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
