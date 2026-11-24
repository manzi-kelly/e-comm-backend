package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/fulfillment"
	"autivashop/backend/internal/middleware"
)

const maxFulfillmentBodyBytes = 8 << 10

type fulfillmentHandlers struct {
	svc    *fulfillment.Service
	logger *slog.Logger
}

type transitionRequest struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	Note            string `json:"note,omitempty"`
	IdempotencyKey  string `json:"idempotencyKey"`
}

type customerFulfillmentHandlers struct {
	svc *fulfillment.Service
}

func (h *fulfillmentHandlers) getState(w http.ResponseWriter, r *http.Request) {
	state, err := h.svc.GetState(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin fulfillment state is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, state)
}

func (h *fulfillmentHandlers) getHistory(w http.ResponseWriter, r *http.Request) {
	history, err := h.svc.GetHistory(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin fulfillment history is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"items": history})
}

func (h *fulfillmentHandlers) getConflicts(w http.ResponseWriter, r *http.Request) {
	conflicts, err := h.svc.GetConflicts(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin fulfillment conflicts are unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"items": conflicts})
}

func (h *fulfillmentHandlers) transition(event string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		if !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
			middleware.WriteError(w, r, http.StatusUnsupportedMediaType,
				middleware.CodeUnsupportedMediaType, "Content-Type must be application/json.", nil)
			return
		}
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, maxFulfillmentBodyBytes+1))
		if readErr != nil {
			middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
				"Request body could not be read.", nil)
			return
		}
		if len(raw) > maxFulfillmentBodyBytes {
			middleware.WriteError(w, r, http.StatusRequestEntityTooLarge, middleware.CodePayloadTooLarge,
				"Request body is too large.", nil)
			return
		}
		var req transitionRequest
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
				"Invalid request body.", nil)
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
				"Invalid request body.", nil)
			return
		}
		user, session, ok := h.adminPrincipal(r)
		if !ok {
			middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
				"Authentication required.", nil)
			return
		}
		actor := fulfillment.Actor{
			AdminID:   user.ID,
			SessionID: session.ID,
			Email:     user.Email,
			Role:      string(user.Role),
			IP:        auth.ClientIP(r),
			UserAgent: r.UserAgent(),
		}
		result, err := h.svc.Transition(r.Context(), actor, fulfillment.Command{
			OrderID:         r.PathValue("id"),
			Event:           event,
			ExpectedVersion: req.ExpectedVersion,
			Note:            req.Note,
			IdempotencyKey:  req.IdempotencyKey,
		})
		if err != nil {
			h.fail(w, r, err, "Admin fulfillment transition failed.")
			return
		}
		status := http.StatusOK
		if result.Replayed {
			status = http.StatusAccepted
		}
		middleware.WriteJSON(w, status, result)
	}
}

func (h *fulfillmentHandlers) adminPrincipal(r *http.Request) (admin.User, admin.Session, bool) {
	user, userOK := adminUserFromContext(r.Context())
	session, sessionOK := adminSessionFromContext(r.Context())
	if !userOK || !sessionOK {
		return admin.User{}, admin.Session{}, false
	}
	return user, session, true
}

func (h *fulfillmentHandlers) fail(w http.ResponseWriter, r *http.Request, err error, internalMessage string) {
	status, code, message := http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred."
	switch {
	case errors.Is(err, fulfillment.ErrValidation):
		status, code, message = http.StatusUnprocessableEntity, middleware.CodeValidationFailed,
			"Fulfillment input failed validation."
	case errors.Is(err, fulfillment.ErrNotFound):
		status, code, message = http.StatusNotFound, middleware.CodeNotFound, "Fulfillment record not found."
	case errors.Is(err, fulfillment.ErrForbidden):
		status, code, message = http.StatusForbidden, middleware.CodeForbidden,
			"Fulfillment action is not permitted in this environment."
	case errors.Is(err, fulfillment.ErrInvalidTransition), errors.Is(err, fulfillment.ErrStaleVersion),
		errors.Is(err, fulfillment.ErrIdempotencyConflict), errors.Is(err, fulfillment.ErrPaymentNotEligible),
		errors.Is(err, fulfillment.ErrAllocationUnresolved), errors.Is(err, fulfillment.ErrStockUnavailable):
		status, code = http.StatusConflict, middleware.CodeConflict
		switch {
		case errors.Is(err, fulfillment.ErrInvalidTransition):
			message = "Fulfillment transition is not allowed from the current state."
		case errors.Is(err, fulfillment.ErrStaleVersion):
			message = "Fulfillment version is stale; reload the order and retry."
		case errors.Is(err, fulfillment.ErrIdempotencyConflict):
			message = "Idempotency key was reused with a different request."
		case errors.Is(err, fulfillment.ErrPaymentNotEligible):
			message = "Payment is not eligible for fulfillment."
		case errors.Is(err, fulfillment.ErrAllocationUnresolved):
			message = "Inventory allocation for this order is unresolved."
		default:
			message = "Stock is unavailable for this order."
		}
	}
	if status == http.StatusInternalServerError && h.logger != nil {
		h.logger.Error(internalMessage, "error", err)
	}
	middleware.WriteError(w, r, status, code, message, nil)
}

func (h *customerFulfillmentHandlers) get(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.CustomerPrincipalFromContext(r.Context())
	if !ok {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Authentication required.", nil)
		return
	}
	view, err := h.svc.CustomerGet(r.Context(), principal.ID, r.PathValue("id"))
	if err != nil {
		status, code, message := http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred."
		switch {
		case errors.Is(err, fulfillment.ErrValidation):
			status, code, message = http.StatusUnprocessableEntity, middleware.CodeValidationFailed,
				"Fulfillment request failed validation."
		case errors.Is(err, fulfillment.ErrNotFound):
			status, code, message = http.StatusNotFound, middleware.CodeNotFound, "Fulfillment record not found."
		}
		middleware.WriteError(w, r, status, code, message, nil)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, view)
}
