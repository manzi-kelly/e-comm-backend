package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/middleware"
	"autivashop/backend/internal/orders"
)

type orderHandlers struct{ svc *orders.Service }

func (h *orderHandlers) create(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !orders.ValidIdempotencyKey(key) {
		middleware.WriteError(w, r, http.StatusBadRequest, middleware.CodeBadRequest,
			"A valid Idempotency-Key header is required.", nil)
		return
	}
	b, ok := jsonBody(w, r)
	if !ok {
		return
	}
	in, fields := orders.Decode(b)
	if len(fields) != 0 {
		middleware.WriteError(w, r, http.StatusUnprocessableEntity,
			middleware.CodeValidationFailed, "Order input failed validation.", fields)
		return
	}
	principal, ok := auth.CustomerPrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	meta := orders.RequestMeta{IP: auth.ClientIP(r), UserAgent: r.UserAgent()}
	order, replay, err := h.svc.Create(r.Context(), principal.ID, in, key, meta)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/orders/"+order.ID)
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	middleware.WriteJSON(w, status, order)
}

func (h *orderHandlers) get(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.CustomerPrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	order, err := h.svc.Get(r.Context(), principal.ID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, order)
}

func (h *orderHandlers) list(w http.ResponseWriter, r *http.Request) {
	page, limit, ok := parseOrderListQuery(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnprocessableEntity,
			middleware.CodeValidationFailed, "Order query failed validation.", nil)
		return
	}
	principal, ok := auth.CustomerPrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		middleware.WriteError(w, r, http.StatusUnauthorized, middleware.CodeUnauthenticated,
			"Customer authentication required.", nil)
		return
	}
	items, hasNext, err := h.svc.List(r.Context(), principal.ID, page, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{
		"items":   items,
		"page":    page,
		"limit":   limit,
		"hasNext": hasNext,
	})
}

func (h *orderHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred."
	switch {
	case errors.Is(err, orders.ErrValidation):
		status, code, message = http.StatusUnprocessableEntity, middleware.CodeValidationFailed, "Order input failed validation."
	case errors.Is(err, orders.ErrNotFound):
		status, code, message = http.StatusNotFound, middleware.CodeNotFound, "Order not found."
	case errors.Is(err, orders.ErrProductNotFound):
		status, code, message = http.StatusUnprocessableEntity, middleware.CodeValidationFailed, "One or more products are not available for ordering."
	case errors.Is(err, orders.ErrStockUnavailable):
		status, code, message = http.StatusConflict, middleware.CodeConflict, "Requested quantity is no longer available."
	case errors.Is(err, orders.ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, middleware.CodeConflict, "Idempotency-Key was reused with a different request."
	case errors.Is(err, orders.ErrConflict):
		status, code, message = http.StatusConflict, middleware.CodeConflict, "The order request conflicts with a concurrent submission. Reload and retry."
	}
	middleware.WriteError(w, r, status, code, message, nil)
}

func parseOrderListQuery(r *http.Request) (int, int, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, 0, false
	}
	for key := range values {
		if len(values[key]) != 1 || (key != "page" && key != "limit") {
			return 0, 0, false
		}
	}
	page, limit := 1, 20
	if raw := values.Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, false
		}
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, false
		}
	}
	if page < 1 || page > 1000 || limit < 1 || limit > 100 {
		return 0, 0, false
	}
	return page, limit, true
}
