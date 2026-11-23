package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/middleware"
)

type adminHandlers struct {
	svc     *admin.Service
	cfg     config.Config
	cookies auth.CookieConfig
	logger  *slog.Logger
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type adminProfileResponse struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Role        admin.Role `json:"role"`
	IsActive    bool       `json:"isActive"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}

type authResponse struct {
	Admin     adminProfileResponse `json:"admin"`
	CSRFToken string               `json:"csrfToken,omitempty"`
}

type unauthenticatedResponse struct {
	CSRFToken string `json:"csrfToken,omitempty"`
}

type logoutAllResponse struct {
	Status       string `json:"status"`
	RevokedCount int64  `json:"revokedCount"`
}

type sessionCheckResponse struct {
	Status string               `json:"status"`
	Admin  adminProfileResponse `json:"admin"`
}

func (h *adminHandlers) login(w http.ResponseWriter, r *http.Request) {
	if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
		return
	}
	var req loginRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest, "Invalid request body.", nil)
		return
	}
	req.Email = admin.NormalizeEmail(req.Email)
	if req.Email == "" || strings.TrimSpace(req.Password) == "" || len(req.Email) > 320 || len(req.Password) > 4096 {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated, "Invalid email or password.", nil)
		return
	}
	meta := admin.LoginMeta{IP: auth.ClientIP(r), UserAgent: r.UserAgent()}
	user, sess, sessionToken, csrfToken, err := h.svc.Login(r.Context(), req.Email, req.Password, meta)
	if err != nil {
		switch {
		case errors.Is(err, admin.ErrRateLimited):
			w.Header().Set("Retry-After", "60")
			middleware.WriteError(w, r, http.StatusTooManyRequests, middleware.CodeRateLimited, "Too many login attempts. Try again later.", nil)
		case errors.Is(err, admin.ErrAccountDisabled), errors.Is(err, admin.ErrInvalidCredentials):
			middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated, "Invalid email or password.", nil)
		default:
			h.logger.Error("admin login failed", "request_id", middleware.RequestIDFromContext(r.Context()))
			middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred.", nil)
		}
		return
	}

	h.cookies.SetSessionCookie(w, sessionToken, sess.ExpiresAt)
	h.cookies.SetCSRFCookie(w, csrfToken, sess.ExpiresAt)
	middleware.WriteJSON(w, http.StatusOK, authResponse{Admin: toProfile(user), CSRFToken: csrfToken})
}

func (h *adminHandlers) me(w http.ResponseWriter, r *http.Request) {
	// Always ensure a CSRF cookie is available for subsequent state-changing requests.
	csrfToken := h.ensureCSRFCookie(w, r)

	user, _, ok := h.authenticateRequest(w, r, csrfToken)
	if !ok {
		return
	}
	middleware.WriteJSON(w, http.StatusOK, authResponse{Admin: toProfile(user), CSRFToken: csrfToken})
}

func (h *adminHandlers) logout(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := h.currentPrincipal(r)
	if !ok {
		h.clearAuthCookies(w)
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated, "Authentication required.", nil)
		return
	}
	if err := h.svc.Logout(r.Context(), user, sess); err != nil {
		h.logger.Error("admin logout failed", "request_id", middleware.RequestIDFromContext(r.Context()))
		middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred.", nil)
		return
	}
	h.clearAuthCookies(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *adminHandlers) logoutAll(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.currentPrincipal(r)
	if !ok {
		h.clearAuthCookies(w)
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated, "Authentication required.", nil)
		return
	}
	count, err := h.svc.LogoutAll(r.Context(), user)
	if err != nil {
		h.logger.Error("admin logout-all failed", "request_id", middleware.RequestIDFromContext(r.Context()))
		middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred.", nil)
		return
	}
	h.clearAuthCookies(w)
	middleware.WriteJSON(w, http.StatusOK, logoutAllResponse{Status: "ok", RevokedCount: count})
}

func (h *adminHandlers) sessionCheck(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.currentPrincipal(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated, "Authentication required.", nil)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, sessionCheckResponse{Status: "authenticated", Admin: toProfile(user)})
}

func (h *adminHandlers) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.RequiresCSRF(r.Method) {
			if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
				return
			}
		}
		csrfToken := h.ensureCSRFCookie(w, r)
		_, _, ok := h.authenticateRequest(w, r, csrfToken)
		if !ok {
			return
		}
		next.ServeHTTP(w, r)
	}
}

func (h *adminHandlers) authenticateRequest(w http.ResponseWriter, r *http.Request, csrfToken string) (admin.User, admin.Session, bool) {
	cookie, err := r.Cookie(h.cookies.SessionName)
	if err != nil || cookie == nil || strings.TrimSpace(cookie.Value) == "" {
		h.cookies.ClearSessionCookie(w)
		// Keep/issue CSRF cookie token for login flow; return token in body for cross-origin clients.
		middleware.WriteJSON(w, http.StatusUnauthorized, map[string]any{
			"error": middleware.APIError{
				Code:      middleware.CodeUnauthenticated,
				Message:   "Authentication required.",
				RequestID: middleware.RequestIDFromContext(r.Context()),
			},
			"csrfToken": csrfToken,
		})
		return admin.User{}, admin.Session{}, false
	}
	user, sess, err := h.svc.Authenticate(r.Context(), cookie.Value)
	if err != nil {
		// Preserve the CSRF bootstrap cookie returned with this response so the
		// client can log in again after an expired or revoked session.
		h.cookies.ClearSessionCookie(w)
		middleware.WriteJSON(w, http.StatusUnauthorized, map[string]any{
			"error": middleware.APIError{
				Code:      middleware.CodeUnauthenticated,
				Message:   "Authentication required.",
				RequestID: middleware.RequestIDFromContext(r.Context()),
			},
			"csrfToken": csrfToken,
		})
		return admin.User{}, admin.Session{}, false
	}
	principal := auth.AdminPrincipal{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        string(user.Role),
		IsActive:    user.IsActive,
		SessionID:   sess.ID,
	}
	ctx := auth.WithPrincipal(r.Context(), principal)
	ctx = contextWithAdmin(ctx, user, sess)
	*r = *r.WithContext(ctx)
	return user, sess, true
}

func (h *adminHandlers) currentPrincipal(r *http.Request) (admin.User, admin.Session, bool) {
	user, ok := adminUserFromContext(r.Context())
	if !ok {
		return admin.User{}, admin.Session{}, false
	}
	sess, ok := adminSessionFromContext(r.Context())
	if !ok {
		return admin.User{}, admin.Session{}, false
	}
	return user, sess, true
}

func (h *adminHandlers) requireSafeOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !auth.ValidateOrigin(r, h.cfg.CORS.AllowedOrigins) {
		middleware.WriteError(w, r, http.StatusForbidden, middleware.CodeForbidden, "Origin is not allowed.", nil)
		return false
	}
	return true
}

func (h *adminHandlers) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	if !auth.RequiresCSRF(r.Method) {
		return true
	}
	if !auth.ValidCSRFToken(r, h.cookies.CSRFName, h.cfg.Auth.CSRFHeaderName) {
		middleware.WriteError(w, r, http.StatusForbidden, middleware.CodeForbidden, "CSRF validation failed.", nil)
		return false
	}
	return true
}

func (h *adminHandlers) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(h.cookies.CSRFName); err == nil && c != nil && strings.TrimSpace(c.Value) != "" {
		return c.Value
	}
	token, err := auth.GenerateToken(32)
	if err != nil {
		return ""
	}
	// Use session TTL as a reasonable lifetime for anonymous pre-auth CSRF tokens.
	h.cookies.SetCSRFCookie(w, token, time.Now().Add(h.cfg.Auth.SessionTTL))
	return token
}

func (h *adminHandlers) clearAuthCookies(w http.ResponseWriter) {
	h.cookies.ClearSessionCookie(w)
	h.cookies.ClearCSRFCookie(w)
}

func toProfile(u admin.User) adminProfileResponse {
	return adminProfileResponse{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		IsActive:    u.IsActive,
		LastLoginAt: u.LastLoginAt,
	}
}

// context helpers for full admin records (internal use).
type adminUserContextKey struct{}
type adminSessionContextKey struct{}

func contextWithAdmin(ctx context.Context, user admin.User, session admin.Session) context.Context {
	ctx = context.WithValue(ctx, adminUserContextKey{}, user)
	ctx = context.WithValue(ctx, adminSessionContextKey{}, session)
	return ctx
}

func adminUserFromContext(ctx context.Context) (admin.User, bool) {
	u, ok := ctx.Value(adminUserContextKey{}).(admin.User)
	return u, ok
}

func adminSessionFromContext(ctx context.Context) (admin.Session, bool) {
	s, ok := ctx.Value(adminSessionContextKey{}).(admin.Session)
	return s, ok
}
