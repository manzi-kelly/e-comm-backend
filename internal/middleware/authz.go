package middleware

import (
	"net/http"

	"autivashop/backend/internal/auth"
)

// RequireAuthenticated rejects requests without a valid authenticated principal in context.
func RequireAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.PrincipalFromContext(r.Context()); !ok {
			WriteError(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Authentication required.", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireActiveAccount rejects authenticated principals whose account is not active.
func RequireActiveAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			WriteError(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Authentication required.", nil)
			return
		}
		if !p.IsActive {
			WriteError(w, r, http.StatusForbidden, CodeForbidden, "Administrator account is inactive.", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRoles rejects authenticated principals whose role is not in the allowed set.
func RequireRoles(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFromContext(r.Context())
			if !ok {
				WriteError(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Authentication required.", nil)
				return
			}
			if _, ok := allowed[p.Role]; !ok {
				WriteError(w, r, http.StatusForbidden, CodeForbidden, "Insufficient permissions.", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
