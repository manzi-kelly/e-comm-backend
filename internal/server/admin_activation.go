package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"autivashop/backend/internal/adminactivation"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/middleware"
	"autivashop/backend/internal/product"
)

// activationHandlers exposes only the three narrowly scoped, public activation
// operations. It never issues administrator sessions and never exposes an
// account enumeration or authorized-email change endpoint.
type activationHandlers struct {
	svc     *adminactivation.Service
	cfg     config.Config
	cookies auth.CookieConfig
	logger  *slog.Logger
}

type activationRequestCodeRequest struct {
	Email string `json:"email"`
}

type activationVerifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type activationCompleteRequest struct {
	Email           string `json:"email"`
	ActivationToken string `json:"activationToken"`
	Password        string `json:"password"`
}

type activationStatusResponse struct {
	Status string `json:"status"`
}

type activationVerifyResponse struct {
	Status          string `json:"status"`
	ActivationToken string `json:"activationToken"`
}

type activationCompleteResponse struct {
	Status string               `json:"status"`
	Admin  adminProfileResponse `json:"admin"`
}

func (h *activationHandlers) requestCode(w http.ResponseWriter, r *http.Request) {
	if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
		return
	}
	if h.svc == nil {
		h.unconfigured(w, r)
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var req activationRequestCodeRequest
	if product.StrictJSON(b, &req) != nil || strings.TrimSpace(req.Email) == "" || len(req.Email) > 320 {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest, "Invalid request body.", nil)
		return
	}
	if err := h.svc.RequestCode(r.Context(), req.Email, auth.ClientIP(r)); err != nil {
		switch {
		case errors.Is(err, adminactivation.ErrRateLimited):
			w.Header().Set("Retry-After", strconv.FormatInt(activationRetryAfter(err), 10))
			middleware.WriteError(w, r, http.StatusTooManyRequests, middleware.CodeRateLimited,
				"Too many activation requests. Try again later.", nil)
		case errors.Is(err, adminactivation.ErrEmailDelivery):
			middleware.WriteError(w, r, http.StatusBadGateway, middleware.CodeStorageUnavailable,
				"The activation email could not be delivered. No code was sent. Try again later.", nil)
		default:
			h.logger.Error("admin activation code request failed", "request_id", middleware.RequestIDFromContext(r.Context()))
			middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError,
				"An internal error occurred.", nil)
		}
		return
	}
	// Generic success: authorized, unauthorized and unprovisioned addresses
	// are indistinguishable to the caller. Only authorized addresses receive mail.
	middleware.WriteJSON(w, http.StatusAccepted, activationStatusResponse{Status: "accepted"})
}

func (h *activationHandlers) verifyCode(w http.ResponseWriter, r *http.Request) {
	if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
		return
	}
	if h.svc == nil {
		h.unconfigured(w, r)
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var req activationVerifyRequest
	if product.StrictJSON(b, &req) != nil || strings.TrimSpace(req.Email) == "" ||
		len(req.Email) > 320 || req.Code == "" || len(req.Code) > 16 {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest, "Invalid activation request.", nil)
		return
	}
	token, err := h.svc.VerifyCode(r.Context(), req.Email, req.Code, auth.ClientIP(r))
	if err != nil {
		switch {
		case errors.Is(err, adminactivation.ErrRateLimited):
			w.Header().Set("Retry-After", strconv.FormatInt(activationRetryAfter(err), 10))
			middleware.WriteError(w, r, http.StatusTooManyRequests, middleware.CodeRateLimited,
				"Too many verification attempts. Try again later.", nil)
		case errors.Is(err, adminactivation.ErrInvalidCode):
			middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
				"The activation code is invalid or has expired.", nil)
		default:
			h.logger.Error("admin activation verification failed", "request_id", middleware.RequestIDFromContext(r.Context()))
			middleware.WriteError(w, r, http.StatusInternalServerError, middleware.CodeInternalError,
				"An internal error occurred.", nil)
		}
		return
	}
	middleware.WriteJSON(w, http.StatusOK, activationVerifyResponse{Status: "verified", ActivationToken: token})
}

func (h *activationHandlers) complete(w http.ResponseWriter, r *http.Request) {
	if !h.requireSafeOrigin(w, r) || !h.requireCSRF(w, r) {
		return
	}
	if h.svc == nil {
		h.unconfigured(w, r)
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	var req activationCompleteRequest
	if product.StrictJSON(b, &req) != nil || strings.TrimSpace(req.Email) == "" ||
		len(req.Email) > 320 || req.ActivationToken == "" || len(req.ActivationToken) > 4096 ||
		len(req.Password) > 4096 {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest, "Invalid activation request.", nil)
		return
	}
	user, err := h.svc.Complete(r.Context(), req.Email, req.ActivationToken, req.Password, auth.ClientIP(r))
	if err != nil {
		switch {
		case errors.Is(err, adminactivation.ErrRateLimited):
			w.Header().Set("Retry-After", strconv.FormatInt(activationRetryAfter(err), 10))
			middleware.WriteError(w, r, http.StatusTooManyRequests, middleware.CodeRateLimited,
				"Too many activation attempts. Try again later.", nil)
		case errors.Is(err, adminactivation.ErrPasswordWeak):
			middleware.WriteError(w, r, http.StatusUnprocessableEntity, middleware.CodeValidationFailed,
				"Administrator password failed validation.", map[string][]string{
					"password": {"Use at least 12 characters and meet the administrator password policy."},
				})
		default:
			middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
				"Administrator activation could not be completed.", nil)
		}
		return
	}
	middleware.WriteJSON(w, http.StatusCreated, activationCompleteResponse{
		Status: "activated",
		Admin:  toProfile(user),
	})
}

// activationRetryAfter prefers the service's measured remaining lockout and
// falls back to one minute only for an unexpected untyped rate-limit error.
func activationRetryAfter(err error) int64 {
	if seconds := adminactivation.RetryAfterSeconds(err); seconds > 0 {
		return seconds
	}
	return 60
}

func (h *activationHandlers) unconfigured(w http.ResponseWriter, r *http.Request) {
	message := "Administrator activation is not configured on this server."
	if issues := h.cfg.AdminActivation.SetupIssues; len(issues) > 0 {
		message += " Required setup: " + strings.Join(issues, ", ") + "."
	}
	middleware.WriteError(w, r, http.StatusServiceUnavailable, middleware.CodeStorageUnavailable, message, nil)
}

func (h *activationHandlers) requireSafeOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !auth.ValidateOrigin(r, h.cfg.CORS.AllowedOrigins) {
		middleware.WriteError(w, r, http.StatusForbidden, middleware.CodeForbidden, "Origin is not allowed.", nil)
		return false
	}
	return true
}

func (h *activationHandlers) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	if !auth.RequiresCSRF(r.Method) {
		return true
	}
	if !auth.ValidCSRFToken(r, h.cookies.CSRFName, h.cfg.Auth.CSRFHeaderName) {
		middleware.WriteError(w, r, http.StatusForbidden, middleware.CodeForbidden, "CSRF validation failed.", nil)
		return false
	}
	return true
}
