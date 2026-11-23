package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/middleware"
	"autivashop/backend/internal/payment"
	"autivashop/backend/internal/product"
)

// paymentHandlers exposes the local, mock-only payment surface. All amounts,
// order ownership and state transitions are decided by payment.Service; these
// handlers only decode input, enforce transport security and map errors.
type paymentHandlers struct {
	svc    *payment.Service
	logger *slog.Logger
}

type initiatePaymentRequest struct {
	Phone string `json:"phone"`
}

type simulatePaymentRequest struct {
	Outcome string `json:"outcome"`
}

// decodeStrictPaymentBody rejects unknown/duplicate fields and requires the
// named fields to be present and non-null.
func decodeStrictPaymentBody(w http.ResponseWriter, r *http.Request, dst any, required ...string) bool {
	b, ok := jsonBody(w, r)
	if !ok {
		return false
	}
	var fields map[string]json.RawMessage
	if product.StrictJSON(b, &fields) != nil || product.StrictJSON(b, dst) != nil {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
			"Invalid request body.", nil)
		return false
	}
	for _, key := range required {
		value, present := fields[key]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
				"Invalid request body.", nil)
			return false
		}
	}
	return true
}

func currentCustomerID(r *http.Request) (string, bool) {
	principal, ok := auth.CustomerPrincipalFromContext(r.Context())
	if !ok || principal.ID == "" || principal.SessionID == "" {
		return "", false
	}
	return principal.ID, true
}

func (h *paymentHandlers) initiate(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !payment.ValidIdempotencyKey(key) {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
			"A valid Idempotency-Key header is required.", nil)
		return
	}
	var in initiatePaymentRequest
	if !decodeStrictPaymentBody(w, r, &in, "phone") {
		return
	}
	customerID, ok := currentCustomerID(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	orderID := r.PathValue("id")
	attempt, replay, err := h.svc.Initiate(r.Context(), customerID, orderID, key, in.Phone)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/orders/"+orderID+"/payments/"+attempt.ID)
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	middleware.WriteJSON(w, status, attempt)
}

func (h *paymentHandlers) listForOrder(w http.ResponseWriter, r *http.Request) {
	customerID, ok := currentCustomerID(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	items, err := h.svc.ListForOrder(r.Context(), customerID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *paymentHandlers) get(w http.ResponseWriter, r *http.Request) {
	customerID, ok := currentCustomerID(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	attempt, err := h.svc.Get(r.Context(), customerID, r.PathValue("id"), r.PathValue("paymentId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, attempt)
}

// simulate is a development/test-only local control. It is not registered in
// production and never claims that a phone prompt or real payment occurred.
func (h *paymentHandlers) simulate(w http.ResponseWriter, r *http.Request) {
	var in simulatePaymentRequest
	if !decodeStrictPaymentBody(w, r, &in, "outcome") {
		return
	}
	customerID, ok := currentCustomerID(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	attempt, err := h.svc.Simulate(r.Context(), customerID, r.PathValue("paymentId"), strings.ToLower(strings.TrimSpace(in.Outcome)))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, attempt)
}

func (h *paymentHandlers) adminList(w http.ResponseWriter, r *http.Request) {
	filter, ok := parsePaymentAdminQuery(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnprocessableEntity,
			middleware.CodeValidationFailed, "Payment query failed validation.", nil)
		return
	}
	items, hasNext, err := h.svc.AdminList(r.Context(), filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{
		"items":   items,
		"page":    filter.Page,
		"limit":   filter.Limit,
		"hasNext": hasNext,
	})
}

func (h *paymentHandlers) adminGet(w http.ResponseWriter, r *http.Request) {
	attempt, err := h.svc.AdminGet(r.Context(), r.PathValue("attemptId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, attempt)
}

func (h *paymentHandlers) adminSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := h.svc.Revenue(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, summary)
}

func (h *paymentHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred."
	switch {
	case errors.Is(err, payment.ErrValidation):
		status, code, message = http.StatusUnprocessableEntity, middleware.CodeValidationFailed, "Payment input failed validation."
	case errors.Is(err, payment.ErrNotFound):
		status, code, message = http.StatusNotFound, middleware.CodeNotFound, "Payment attempt not found."
	case errors.Is(err, payment.ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, middleware.CodeConflict, "Idempotency-Key was reused with a different phone number."
	case errors.Is(err, payment.ErrReservationExpired):
		status, code, message = http.StatusConflict, middleware.CodeConflict, "The order reservation expired before payment."
	case errors.Is(err, payment.ErrAlreadyPaid), errors.Is(err, payment.ErrInvalidTransition),
		errors.Is(err, payment.ErrConflict), errors.Is(err, payment.ErrStockUnavailable):
		status, code, message = http.StatusConflict, middleware.CodeConflict, "The payment request conflicts with the current order state."
	case errors.Is(err, payment.ErrProviderUnavailable), errors.Is(err, payment.ErrLiveNotImplemented):
		status, code, message = http.StatusServiceUnavailable, middleware.CodeStorageUnavailable, "Payment provider is not available."
	}
	// Internal detail stays in the server log; clients only receive a generic error.
	if status == http.StatusInternalServerError && h.logger != nil {
		h.logger.Error("payment request failed", "error", err)
	}
	middleware.WriteError(w, r, status, code, message, nil)
}

func parsePaymentAdminQuery(r *http.Request) (payment.Filter, bool) {
	filter := payment.Filter{Page: 1, Limit: 20}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return filter, false
	}
	for key := range values {
		if len(values[key]) != 1 || !strings.Contains(
			"|page|limit|state|provider|orderReference|fromDate|toDate|", "|"+key+"|") {
			return filter, false
		}
	}
	integers := map[string]*int{"page": &filter.Page, "limit": &filter.Limit}
	for key, dst := range integers {
		if raw := values.Get(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return filter, false
			}
			*dst = value
		}
	}
	filter.State = values.Get("state")
	filter.Provider = strings.ToLower(values.Get("provider"))
	filter.OrderReference = strings.ToUpper(values.Get("orderReference"))
	filter.FromDate = values.Get("fromDate")
	filter.ToDate = values.Get("toDate")
	return filter, true
}
