package middleware

import (
	"net/http"

	"autivashop/backend/internal/auth"
)

// RequireCustomer rejects requests without a validated customer principal.
// It intentionally ignores the admin principal context: the two identity
// types are never interchangeable.
func RequireCustomer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.CustomerPrincipalFromContext(r.Context())
		if !ok || p.ID == "" || p.SessionID == "" {
			WriteError(w, r, http.StatusUnauthorized, CodeUnauthenticated,
				"Customer authentication required.", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
