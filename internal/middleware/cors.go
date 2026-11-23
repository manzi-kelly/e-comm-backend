package middleware

import (
	"net/http"
	"strings"
)

const (
	allowedMethods         = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	allowedRequestHeaders  = "Content-Type, Authorization, X-Request-ID, Idempotency-Key, X-CSRF-Token, If-Match"
	exposedResponseHeaders = "X-Request-ID, ETag"
	preflightMaxAge        = "600"
)

// CORS permits only exact configured origins. Wildcards are rejected by config
// validation. Disallowed origins receive no CORS grant (and a 403 on preflight).
func CORS(allowedOrigins []string, allowCredentials bool) Middleware {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[origin] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			if _, ok := allowed[origin]; !ok {
				if isPreflight(r) {
					WriteError(w, r, http.StatusForbidden, CodeForbidden,
						"Origin is not allowed by the CORS policy.", nil)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", allowedMethods)
			h.Set("Access-Control-Allow-Headers", allowedRequestHeaders)
			h.Set("Access-Control-Expose-Headers", exposedResponseHeaders)
			h.Set("Access-Control-Max-Age", preflightMaxAge)
			h.Add("Vary", "Origin")
			if allowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}

			if isPreflight(r) {
				method := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
				if !methodAllowed(method) {
					WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
						"Requested method is not allowed by the CORS policy.", nil)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

func methodAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}
