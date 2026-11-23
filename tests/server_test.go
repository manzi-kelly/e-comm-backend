package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"autivashop/backend/internal/config"
	"autivashop/backend/internal/server"
)

type stubPinger struct {
	err error
}

func (s stubPinger) Ping(_ context.Context) error { return s.err }

func testConfig(t *testing.T, overrides map[string]string) config.Config {
	t.Helper()
	values := map[string]string{
		"APP_ENV":      "test",
		"DATABASE_URL": "postgres://app:pw@127.0.0.1:5432/autivashop_test?sslmode=disable",
	}
	for key, value := range overrides {
		values[key] = value
	}
	cfg, err := config.Load(envFrom(values))
	if err != nil {
		t.Fatalf("test configuration invalid: %v", err)
	}
	return cfg
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testServer(t *testing.T, cfg config.Config, db server.Pinger) http.Handler {
	t.Helper()
	return server.New(cfg, db, testLogger()).Handler()
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("could not read response: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %s", body)
	}
	return parsed
}

func TestHealthEndpoint(t *testing.T) {
	handler := testServer(t, testConfig(t, nil), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec.Result())
	if body["status"] != "ok" || body["service"] != "autivashop-api" {
		t.Fatalf("unexpected health payload: %v", body)
	}
	raw := rec.Body.String()
	for _, leaked := range []string{"password", "pw@", "DATABASE_URL", "sslmode"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("health response leaked %q", leaked)
		}
	}
}

func TestReadyEndpoint(t *testing.T) {
	t.Run("database available", func(t *testing.T) {
		handler := testServer(t, testConfig(t, nil), stubPinger{})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := decodeBody(t, rec.Result())
		if body["status"] != "ready" || body["database"] != "connected" {
			t.Fatalf("unexpected readiness payload: %v", body)
		}
	})

	for name, db := range map[string]server.Pinger{
		"database unavailable":    stubPinger{err: errors.New("password=SuperSecret123")},
		"database not configured": nil,
	} {
		t.Run(name, func(t *testing.T) {
			handler := testServer(t, testConfig(t, nil), db)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			body := decodeBody(t, rec.Result())
			envelope, ok := body["error"].(map[string]any)
			if !ok || envelope["code"] != "storage_unavailable" {
				t.Fatalf("unexpected error envelope: %v", body)
			}
			raw := rec.Body.String()
			if strings.Contains(raw, "SuperSecret123") || strings.Contains(raw, "password=") {
				t.Fatal("readiness response leaked database details")
			}
		})
	}
}

type stubListener struct {
	closed chan struct{}
	once   sync.Once
}

func (l *stubListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *stubListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *stubListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestGracefulShutdown(t *testing.T) {
	cfg := testConfig(t, map[string]string{"SHUTDOWN_TIMEOUT": "2s"})
	srv := server.New(cfg, stubPinger{}, testLogger())
	listener := &stubListener{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- srv.Serve(ctx, listener) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("graceful shutdown did not complete")
	}
}

func TestUnknownRouteAndMethodNotAllowed(t *testing.T) {
	handler := testServer(t, testConfig(t, nil), stubPinger{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", rec.Code)
	}
	envelope := decodeBody(t, rec.Result())["error"].(map[string]any)
	if envelope["code"] != "not_found" {
		t.Fatalf("unexpected code: %v", envelope["code"])
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d, want 405", rec.Code)
	}
	envelope = decodeBody(t, rec.Result())["error"].(map[string]any)
	if envelope["code"] != "method_not_allowed" {
		t.Fatalf("unexpected code: %v", envelope["code"])
	}
}
