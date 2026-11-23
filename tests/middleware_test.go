package tests

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autivashop/backend/internal/middleware"
)

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"requestId"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error envelope: %s", rec.Body.String())
	}
	return envelope.Error.Code
}

func errorRequestID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			RequestID string `json:"requestId"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error envelope: %s", rec.Body.String())
	}
	return envelope.Error.RequestID
}

func TestRequestID(t *testing.T) {
	handler := testServer(t, testConfig(t, nil), stubPinger{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil))
	generated := rec.Header().Get("X-Request-ID")
	if !strings.HasPrefix(generated, "req_") || len(generated) < 10 {
		t.Fatalf("generated request ID = %q", generated)
	}
	if errorRequestID(t, rec) != generated {
		t.Fatal("error envelope requestId does not match X-Request-ID header")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	req.Header.Set("X-Request-ID", "req_client123")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") != "req_client123" {
		t.Fatal("valid inbound request ID was not preserved")
	}
	if errorRequestID(t, rec) != "req_client123" {
		t.Fatal("error envelope did not use the inbound request ID")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	req.Header.Set("X-Request-ID", "bad id with spaces!")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") == "bad id with spaces!" {
		t.Fatal("unsafe inbound request ID was accepted")
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler := testServer(t, testConfig(t, nil), stubPinger{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	h := rec.Header()
	checks := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cache-Control":                "no-store",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for key, want := range checks {
		if got := h.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("missing restrictive Content-Security-Policy")
	}
	if !strings.Contains(h.Get("Content-Type"), "application/json") {
		t.Fatal("JSON content type missing")
	}
}

func TestCORS(t *testing.T) {
	handler := testServer(t, testConfig(t, nil), stubPinger{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("allowed origin header = %q", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Fatal("Vary: Origin missing")
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "If-Match") || !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "ETag") {
		t.Fatal("product version headers are not available to allowed browser origins")
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") == "true" {
		t.Fatal("credentials must not be allowed by default")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin received CORS grant: %q", got)
	}

	req = httptest.NewRequest(http.MethodOptions, "/api/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight status = %d, want 204", rec.Code)
	}

	req = httptest.NewRequest(http.MethodOptions, "/api/v1/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden" {
		t.Fatalf("disallowed preflight = %d / %s", rec.Code, errorCode(t, rec))
	}

	req = httptest.NewRequest(http.MethodOptions, "/api/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "TRACE")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("disallowed preflight method status = %d, want 405", rec.Code)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	cfg := testConfig(t, map[string]string{"MAX_REQUEST_BODY_BYTES": "16"})
	handler := testServer(t, cfg, stubPinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/health", strings.NewReader(strings.Repeat("x", 32)))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Fatalf("oversized body = %d / %s", rec.Code, errorCode(t, rec))
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/health", strings.NewReader("{}"))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("small body should reach routing; status = %d", rec.Code)
	}
}

func TestPanicRecovery(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	handler := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom-secret-value")
		}),
		middleware.RequestID,
		middleware.Recover(logger),
		middleware.SecurityHeaders,
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, want 500", rec.Code)
	}
	if errorCode(t, rec) != "internal_error" {
		t.Fatalf("unexpected panic error code: %s", errorCode(t, rec))
	}
	if strings.Contains(rec.Body.String(), "boom-secret-value") || strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatal("panic response leaked internals")
	}
	if rec.Header().Get("X-Request-ID") != errorRequestID(t, rec) {
		t.Fatal("panic error envelope is missing the request ID")
	}
}
