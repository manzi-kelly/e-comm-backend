package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"autivashop/backend/internal/adminreport"
	"autivashop/backend/internal/middleware"
)

// adminReportHandlers exposes read-only Admin Orders and Dashboard data. All
// service calls are behind the Admin session, active-account and role chain
// registered in routes.go; these handlers perform no state changes.
type adminReportHandlers struct {
	svc    *adminreport.Service
	logger *slog.Logger
}

func (h *adminReportHandlers) listOrders(w http.ResponseWriter, r *http.Request) {
	filter, ok := parseAdminOrderQuery(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnprocessableEntity,
			middleware.CodeValidationFailed, "Order query failed validation.", nil)
		return
	}
	page, err := h.svc.ListOrders(r.Context(), filter)
	if err != nil {
		h.fail(w, r, err, "Admin order list is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, page)
}

func (h *adminReportHandlers) getOrder(w http.ResponseWriter, r *http.Request) {
	detail, err := h.svc.GetOrder(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin order detail is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, detail)
}

func (h *adminReportHandlers) getItems(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.GetItems(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin order items are unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *adminReportHandlers) getContact(w http.ResponseWriter, r *http.Request) {
	contact, err := h.svc.GetContact(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin order contact is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, contact)
}

func (h *adminReportHandlers) getDelivery(w http.ResponseWriter, r *http.Request) {
	delivery, err := h.svc.GetDelivery(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin order delivery is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, delivery)
}

func (h *adminReportHandlers) getPayment(w http.ResponseWriter, r *http.Request) {
	payment, err := h.svc.GetPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin order payment is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, payment)
}

func (h *adminReportHandlers) getFulfillment(w http.ResponseWriter, r *http.Request) {
	fulfillment, err := h.svc.GetFulfillment(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin order fulfillment is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, fulfillment)
}

func (h *adminReportHandlers) getAttempts(w http.ResponseWriter, r *http.Request) {
	page, limit, ok := parseAttemptPageQuery(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnprocessableEntity,
			middleware.CodeValidationFailed, "Payment attempt query failed validation.", nil)
		return
	}
	result, err := h.svc.GetAttempts(r.Context(), r.PathValue("id"), page, limit)
	if err != nil {
		h.fail(w, r, err, "Admin payment attempts are unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, result)
}

func (h *adminReportHandlers) getAttemptSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := h.svc.GetAttemptSummary(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err, "Admin payment attempt summary is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, summary)
}

func (h *adminReportHandlers) dashboard(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseRecentLimitQuery(r)
	if !ok {
		middleware.WriteError(w, r, http.StatusUnprocessableEntity,
			middleware.CodeValidationFailed, "Dashboard query failed validation.", nil)
		return
	}
	summary, err := h.svc.Dashboard(r.Context(), limit)
	if err != nil {
		h.fail(w, r, err, "Admin dashboard is unavailable.")
		return
	}
	middleware.WriteJSON(w, http.StatusOK, summary)
}

func (h *adminReportHandlers) fail(w http.ResponseWriter, r *http.Request, err error, internalMessage string) {
	status, code, message := http.StatusInternalServerError, middleware.CodeInternalError, "An internal error occurred."
	switch {
	case errors.Is(err, adminreport.ErrValidation):
		status, code, message = http.StatusUnprocessableEntity, middleware.CodeValidationFailed, "Admin report input failed validation."
	case errors.Is(err, adminreport.ErrNotFound):
		status, code, message = http.StatusNotFound, middleware.CodeNotFound, "Admin order not found."
	}
	if status == http.StatusInternalServerError && h.logger != nil {
		h.logger.Error(internalMessage, "error", err)
	}
	middleware.WriteError(w, r, status, code, message, nil)
}

func parseAdminOrderQuery(r *http.Request) (adminreport.OrderFilter, bool) {
	filter := adminreport.OrderFilter{Page: 1, Limit: 20}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return filter, false
	}
	allowed := "|page|limit|search|orderReference|orderStatus|paymentStatus|fulfillmentStatus|localDelivery|from|to|fromDate|toDate|sort|"
	for key := range values {
		if len(values[key]) != 1 || !strings.Contains(allowed, "|"+key+"|") {
			return filter, false
		}
	}
	integers := map[string]*int{"page": &filter.Page, "limit": &filter.Limit}
	for key, dst := range integers {
		if raw, present := values[key]; present {
			value, err := strconv.Atoi(raw[0])
			if err != nil {
				return filter, false
			}
			*dst = value
		}
	}
	if _, fromAlias := values["from"]; fromAlias {
		if _, dateAlias := values["fromDate"]; dateAlias {
			return filter, false
		}
		filter.FromDate = values.Get("from")
	} else {
		filter.FromDate = values.Get("fromDate")
	}
	if _, toAlias := values["to"]; toAlias {
		if _, dateAlias := values["toDate"]; dateAlias {
			return filter, false
		}
		filter.ToDate = values.Get("to")
	} else {
		filter.ToDate = values.Get("toDate")
	}
	filter.Search = strings.TrimSpace(values.Get("search"))
	filter.OrderReference = strings.ToUpper(values.Get("orderReference"))
	filter.OrderStatus = values.Get("orderStatus")
	filter.PaymentStatus = values.Get("paymentStatus")
	filter.FulfillmentStatus = values.Get("fulfillmentStatus")
	filter.LocalDelivery = values.Get("localDelivery")
	filter.Sort = values.Get("sort")
	return filter, true
}

func parseAttemptPageQuery(r *http.Request) (int, int, bool) {
	page, limit := 1, 20
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, 0, false
	}
	for key := range values {
		if len(values[key]) != 1 || (key != "page" && key != "limit") {
			return 0, 0, false
		}
	}
	for key, dst := range map[string]*int{"page": &page, "limit": &limit} {
		if raw, present := values[key]; present {
			value, err := strconv.Atoi(raw[0])
			if err != nil {
				return 0, 0, false
			}
			*dst = value
		}
	}
	return page, limit, true
}

func parseRecentLimitQuery(r *http.Request) (int, bool) {
	limit := adminreport.DefaultRecentLimit
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, false
	}
	for key := range values {
		if key != "recentLimit" || len(values[key]) != 1 {
			return 0, false
		}
	}
	if raw, present := values["recentLimit"]; present {
		value, err := strconv.Atoi(raw[0])
		if err != nil {
			return 0, false
		}
		limit = value
	}
	return limit, true
}
