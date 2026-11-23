package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/customer"
	"autivashop/backend/internal/middleware"
	"autivashop/backend/internal/product"
)

// customerHandlers owns the private customer identity surface. It never reads
// or writes admin cookies, context keys, tables or roles.
type customerHandlers struct {
	svc     *customer.Service
	cfg     config.Config
	cookies auth.CookieConfig
	logger  *slog.Logger
}

type customerAuthResponse struct {
	Customer  customer.Profile `json:"customer"`
	CSRFToken string           `json:"csrfToken,omitempty"`
}

func (h *customerHandlers) register(w http.ResponseWriter, r *http.Request) {
	if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var req customer.RegistrationInput
	if product.StrictJSON(b, &req) != nil {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
			"Invalid request body.", nil)
		return
	}
	normalized, fields := customer.ValidateRegistration(req)
	if len(fields) != 0 {
		middleware.WriteError(w, r, http.StatusUnprocessableEntity,
			middleware.CodeValidationFailed, "Customer registration failed validation.", fields)
		return
	}
	meta := customer.LoginMeta{IP: auth.ClientIP(r), UserAgent: r.UserAgent()}
	account, sess, sessionToken, csrfToken, err := h.svc.Register(r.Context(), normalized, meta)
	if err != nil {
		switch {
		case errors.Is(err, customer.ErrDuplicateCustomer):
			middleware.WriteError(w, r, http.StatusConflict, middleware.CodeConflict,
				"An account with those details already exists.", nil)
		case errors.Is(err, customer.ErrRateLimited):
			w.Header().Set("Retry-After", "60")
			middleware.WriteError(w, r, http.StatusTooManyRequests, middleware.CodeRateLimited,
				"Too many registration attempts. Try again later.", nil)
		default:
			h.logger.Error("customer registration failed", "request_id", middleware.RequestIDFromContext(r.Context()))
			middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError,
				"An internal error occurred.", nil)
		}
		return
	}
	h.setAuthCookies(w, sessionToken, csrfToken, sess.ExpiresAt)
	middleware.WriteJSON(w, http.StatusCreated,
		customerAuthResponse{Customer: account.Public(), CSRFToken: csrfToken})
}

func (h *customerHandlers) login(w http.ResponseWriter, r *http.Request) {
	if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var req loginRequest
	if product.StrictJSON(b, &req) != nil {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
			"Invalid request body.", nil)
		return
	}
	email := customer.NormalizeEmail(req.Email)
	if email == "" || strings.TrimSpace(req.Password) == "" || len(email) > 320 || len(req.Password) > 4096 {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Invalid email or password.", nil)
		return
	}
	meta := customer.LoginMeta{IP: auth.ClientIP(r), UserAgent: r.UserAgent()}
	account, sess, sessionToken, csrfToken, err := h.svc.Login(r.Context(), email, req.Password, meta)
	if err != nil {
		switch {
		case errors.Is(err, customer.ErrRateLimited):
			w.Header().Set("Retry-After", "60")
			middleware.WriteError(w, r, http.StatusTooManyRequests, middleware.CodeRateLimited,
				"Too many login attempts. Try again later.", nil)
		case errors.Is(err, customer.ErrInvalidCredentials):
			middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
				"Invalid email or password.", nil)
		default:
			h.logger.Error("customer login failed", "request_id", middleware.RequestIDFromContext(r.Context()))
			middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError,
				"An internal error occurred.", nil)
		}
		return
	}
	h.setAuthCookies(w, sessionToken, csrfToken, sess.ExpiresAt)
	middleware.WriteJSON(w, http.StatusOK,
		customerAuthResponse{Customer: account.Public(), CSRFToken: csrfToken})
}

// me authenticates an existing customer session and always provides a CSRF
// bootstrap token so public registration/login can be performed cross-origin.
func (h *customerHandlers) me(w http.ResponseWriter, r *http.Request) {
	csrfToken := h.ensureCSRFCookie(w, r)
	account, _, ok := h.authenticateRequest(w, r, csrfToken)
	if !ok {
		return
	}
	middleware.WriteJSON(w, http.StatusOK,
		customerAuthResponse{Customer: account.Public(), CSRFToken: csrfToken})
}

func (h *customerHandlers) logout(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := h.currentPrincipal(r)
	if !ok {
		h.clearAuthCookies(w)
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	if err := h.svc.Logout(r.Context(), account, sess); err != nil {
		h.logger.Error("customer logout failed", "request_id", middleware.RequestIDFromContext(r.Context()))
		middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError,
			"An internal error occurred.", nil)
		return
	}
	h.clearAuthCookies(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *customerHandlers) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.RequiresCSRF(r.Method) {
			if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
				return
			}
		}
		csrfToken := h.ensureCSRFCookie(w, r)
		if _, _, ok := h.authenticateRequest(w, r, csrfToken); !ok {
			return
		}
		next(w, r)
	}
}

func (h *customerHandlers) authenticateRequest(w http.ResponseWriter, r *http.Request, csrfToken string) (customer.Customer, customer.Session, bool) {
	cookie, err := r.Cookie(h.cookies.SessionName)
	if err != nil || cookie == nil || strings.TrimSpace(cookie.Value) == "" {
		h.cookies.ClearSessionCookie(w)
		h.writeUnauthenticated(w, r, csrfToken)
		return customer.Customer{}, customer.Session{}, false
	}
	account, sess, err := h.svc.Authenticate(r.Context(), cookie.Value)
	if err != nil {
		h.cookies.ClearSessionCookie(w)
		h.writeUnauthenticated(w, r, csrfToken)
		return customer.Customer{}, customer.Session{}, false
	}

	// Customer identity is written only to customer context keys. Admin
	// authorization middleware cannot observe this principal.
	ctx := auth.WithCustomerPrincipal(r.Context(), auth.CustomerPrincipal{
		ID:        account.ID,
		Name:      account.Name,
		Email:     account.Email,
		PhoneE164: account.PhoneE164,
		SessionID: sess.ID,
	})
	ctx = auth.WithCustomerSession(ctx, sess.ID)
	ctx = contextWithCustomer(ctx, account, sess)
	*r = *r.WithContext(ctx)
	return account, sess, true
}

func (h *customerHandlers) writeUnauthenticated(w http.ResponseWriter, r *http.Request, csrfToken string) {
	middleware.WriteJSON(w, http.StatusUnauthorized, map[string]any{
		"error": middleware.APIError{
			Code:      middleware.CodeUnauthenticated,
			Message:   "Customer authentication required.",
			RequestID: middleware.RequestIDFromContext(r.Context()),
		},
		"csrfToken": csrfToken,
	})
}

func (h *customerHandlers) currentPrincipal(r *http.Request) (customer.Customer, customer.Session, bool) {
	account, ok := customerAccountFromContext(r.Context())
	if !ok {
		return customer.Customer{}, customer.Session{}, false
	}
	sess, ok := customerSessionFromContext(r.Context())
	if !ok {
		return customer.Customer{}, customer.Session{}, false
	}
	return account, sess, true
}

func (h *customerHandlers) requireSafeOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !auth.ValidateOrigin(r, h.cfg.CORS.AllowedOrigins) {
		middleware.WriteError(w, r, http.StatusForbidden, middleware.CodeForbidden,
			"Origin is not allowed.", nil)
		return false
	}
	return true
}

func (h *customerHandlers) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	if !auth.ValidCSRFToken(r, h.cookies.CSRFName, h.cfg.Auth.CSRFHeaderName) {
		middleware.WriteError(w, r, http.StatusForbidden, middleware.CodeForbidden,
			"CSRF validation failed.", nil)
		return false
	}
	return true
}

func (h *customerHandlers) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(h.cookies.CSRFName); err == nil && c != nil && strings.TrimSpace(c.Value) != "" {
		return c.Value
	}
	token, err := auth.GenerateToken(32)
	if err != nil {
		return ""
	}
	h.cookies.SetCSRFCookie(w, token, time.Now().Add(h.cfg.CustomerAuth.SessionTTL))
	return token
}

func (h *customerHandlers) setAuthCookies(w http.ResponseWriter, sessionToken, csrfToken string, expires time.Time) {
	h.cookies.SetSessionCookie(w, sessionToken, expires)
	h.cookies.SetCSRFCookie(w, csrfToken, expires)
}

func (h *customerHandlers) clearAuthCookies(w http.ResponseWriter) {
	h.cookies.ClearSessionCookie(w)
	h.cookies.ClearCSRFCookie(w)
}

type customerAccountContextKey struct{}
type customerSessionContextKey struct{}

func contextWithCustomer(ctx context.Context, account customer.Customer, sess customer.Session) context.Context {
	ctx = context.WithValue(ctx, customerAccountContextKey{}, account)
	return context.WithValue(ctx, customerSessionContextKey{}, sess)
}

func customerAccountFromContext(ctx context.Context) (customer.Customer, bool) {
	account, ok := ctx.Value(customerAccountContextKey{}).(customer.Customer)
	return account, ok
}

func customerSessionFromContext(ctx context.Context) (customer.Session, bool) {
	sess, ok := ctx.Value(customerSessionContextKey{}).(customer.Session)
	return sess, ok
}
