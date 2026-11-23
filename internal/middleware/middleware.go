package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Middleware wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares in order: the first one is the outermost wrapper.
func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// RequestID accepts a bounded, safe inbound ID or issues a new random one.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_unavailable"
	}
	return "req_" + hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

// Recover converts panics into a safe 500 response and logs the stack internally.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			recorder := &statusRecorder{ResponseWriter: w}
			defer func() {
				if value := recover(); value != nil {
					logger.Error("panic recovered",
						"request_id", RequestIDFromContext(r.Context()),
						"method", r.Method,
						"path", r.URL.Path,
						"stack", string(debug.Stack()),
					)
					if recorder.status == 0 {
						WriteError(recorder, r, http.StatusInternalServerError,
							CodeInternalError, "An internal error occurred.", nil)
					}
				}
			}()
			next.ServeHTTP(recorder, r)
		})
	}
}

// Logging emits one structured JSON line per request. Query strings and bodies
// are deliberately excluded because they can carry sensitive data.
func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			recorder := &statusRecorder{ResponseWriter: w}
			started := time.Now()
			next.ServeHTTP(recorder, r)
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(started).Milliseconds(),
			}
			switch {
			case status >= 500:
				logger.Error("request completed", attrs...)
			case status >= 400:
				logger.Warn("request completed", attrs...)
			default:
				logger.Info("request completed", attrs...)
			}
		})
	}
}

// SecurityHeaders applies conservative, API-appropriate response headers.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// RequestTimeout bounds handler work with a context deadline.
func RequestTimeout(timeout time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r.WithContext(ctx))
			if ctx.Err() == context.DeadlineExceeded && recorder.status == 0 {
				WriteError(recorder, r.WithContext(ctx), http.StatusGatewayTimeout,
					CodeRequestTimeout, "The request timed out.", nil)
			}
		})
	}
}

// MethodNotAllowed returns the JSON error envelope for a known path.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET")
	WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
		"Method not allowed for this endpoint.", nil)
}

// Problem is a small helper for safe, unsupported requests.
func Problem(w http.ResponseWriter, r *http.Request, status int, message string) {
	WriteError(w, r, status, CodeBadRequest, fmt.Sprint(message), nil)
}
