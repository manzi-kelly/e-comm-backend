//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autivashop/backend/internal/adminreport"
	"autivashop/backend/internal/orders"
	"autivashop/backend/internal/payment"
	"autivashop/backend/internal/server"
)

type adminReportEnv struct {
	*paymentEnv
	reportSvc *adminreport.Service
}

func setupAdminReports(t *testing.T) *adminReportEnv {
	t.Helper()
	e := setupPayments(t)
	svc := adminreport.NewService(e.db.Pool())
	srv := server.New(e.cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetAdminReportService(svc)
	srv.SetProductService(e.svc)
	srv.SetCustomerService(e.customerSvc)
	srv.SetOrderService(e.orderSvc)
	srv.SetPaymentService(e.paymentSvc)
	e.handler = srv.Handler()
	return &adminReportEnv{paymentEnv: e, reportSvc: svc}
}

func (e *adminReportEnv) reportRequest(t *testing.T, method, path string, body any, authenticated bool) *http.Response {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	if authenticated {
		req.AddCookie(e.session)
		req.AddCookie(e.csrf)
		req.Header.Set("X-CSRF-Token", e.csrf.Value)
	}
	return doReq(e.handler, req).Result()
}

func decodeReportBody[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var value T
	if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return value
}

func requireNoStore(t *testing.T, resp *http.Response) {
	t.Helper()
	if !strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") {
		t.Fatalf("missing no-store: %s", resp.Header.Get("Cache-Control"))
	}
}

func TestPhase10AdminOrdersReadSurfaces(t *testing.T) {
	e := setupAdminReports(t)
	sess := e.registerCustomer(t, "phase10-orders-"+phase06ID("cust")+"@example.test", "Customer-Report-Password-123!")
	p := e.createPublishedProduct(t, 85000, 3)
	order := e.createPaymentOrder(t, sess, p, 1)

	attemptRec := e.initiatePayment(t, sess, order.ID, phase06ID("report-pay"), "0781234567")
	if attemptRec.Code != http.StatusCreated {
		t.Fatalf("initiate payment: %d %s", attemptRec.Code, attemptRec.Body.String())
	}
	attempt := decodePaymentAttempt(t, attemptRec)
	if approved := e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove); approved.Code != http.StatusOK {
		t.Fatalf("approve payment: %d %s", approved.Code, approved.Body.String())
	}

	unauth := e.reportRequest(t, http.MethodGet, "/api/v1/admin/orders", nil, false)
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin orders: %d", unauth.StatusCode)
	}
	unauth.Body.Close()
	customerPrincipal := e.customerRequest(t, http.MethodGet, "/api/v1/admin/orders", nil, sess, "")
	if customerPrincipal.Code != http.StatusUnauthorized {
		t.Fatalf("customer principal accepted by admin orders: %d %s", customerPrincipal.Code, customerPrincipal.Body.String())
	}
	if !strings.Contains(strings.ToLower(customerPrincipal.Result().Header.Get("Cache-Control")), "no-store") {
		t.Fatal("customer principal response missing no-store")
	}

	badQueries := []string{
		"page=0", "limit=101", "unknown=1", "page=1&page=2",
		"sort=drop", "from=2026-99-99", "orderStatus=cancelled",
		"from=2026-01-01&fromDate=2026-01-02",
	}
	for _, query := range badQueries {
		resp := e.reportRequest(t, http.MethodGet, "/api/v1/admin/orders?"+query, nil, true)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("invalid query %q accepted: %d %s", query, resp.StatusCode, body)
		}
		resp.Body.Close()
	}

	listResp := e.reportRequest(t, http.MethodGet,
		"/api/v1/admin/orders?search="+order.Reference+"&paymentStatus=paid&sort=newest&limit=5", nil, true)
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("admin order list: %d %s", listResp.StatusCode, body)
	}
	requireNoStore(t, listResp)
	list := decodeReportBody[adminreport.OrderPage](t, listResp)
	var found bool
	for _, item := range list.Items {
		if item.ID == order.ID {
			found = true
			if item.Reference != order.Reference || item.PaymentStatus != "paid" ||
				item.FulfillmentStatus != "pending" || item.TotalRWF != 85000 || item.ItemCount != 1 {
				t.Fatalf("unexpected admin order summary: %+v", item)
			}
			if item.LatestAttempt == nil || item.LatestAttempt.State != "succeeded" || !item.LatestAttempt.IsMock {
				t.Fatalf("latest attempt missing from list: %+v", item.LatestAttempt)
			}
		}
	}
	if !found {
		t.Fatalf("admin list did not contain the test order: %+v", list.Items)
	}

	detailResp := e.reportRequest(t, http.MethodGet, "/api/v1/admin/orders/"+order.ID, nil, true)
	if detailResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(detailResp.Body)
		t.Fatalf("admin order detail: %d %s", detailResp.StatusCode, body)
	}
	requireNoStore(t, detailResp)
	detailRaw, err := io.ReadAll(detailResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(detailRaw), sess.Account.PhoneE164) {
		t.Fatalf("admin detail leaked an unmasked phone: %s", detailRaw)
	}
	var detail adminreport.OrderDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Order.ID != order.ID || detail.Contact.FullName != sess.Account.FullName ||
		detail.Contact.Email != sess.Account.Email || !strings.Contains(detail.Contact.Phone, "••••") ||
		detail.Delivery.District != "Gasabo" || len(detail.Items) != 1 ||
		detail.Items[0].UnitPriceRWF != 85000 || detail.Payment.Status != "paid" ||
		detail.Fulfillment.Status != "pending" || len(detail.PaymentAttempts) != 1 ||
		detail.PaymentAttemptSummary.SucceededAttempts != 1 ||
		detail.PaymentAttemptSummary.MockSucceededAttempts != 1 ||
		detail.PaymentAttemptSummary.MockRevenueIsReal {
		t.Fatalf("unexpected admin detail: %+v", detail)
	}

	subresources := map[string]func(*http.Response){
		"/items": func(resp *http.Response) {
			body := decodeReportBody[struct {
				Items []adminreport.Item `json:"items"`
			}](t, resp)
			if len(body.Items) != 1 || body.Items[0].SKU == "" || body.Items[0].UnitPriceRWF != 85000 {
				t.Fatalf("unexpected items projection: %+v", body.Items)
			}
		},
		"/contact": func(resp *http.Response) {
			body := decodeReportBody[adminreport.Contact](t, resp)
			if body.Email != sess.Account.Email || !strings.Contains(body.PhoneE164, "••••") {
				t.Fatalf("unexpected contact projection: %+v", body)
			}
		},
		"/delivery": func(resp *http.Response) {
			body := decodeReportBody[adminreport.Delivery](t, resp)
			if body.Country != "RW" || body.Province != "Kigali" || body.AddressLine != "KG 123 St" {
				t.Fatalf("unexpected delivery projection: %+v", body)
			}
		},
		"/payment": func(resp *http.Response) {
			body := decodeReportBody[adminreport.Payment](t, resp)
			if body.Status != "paid" || body.AttemptSummary.SucceededAttempts != 1 {
				t.Fatalf("unexpected payment projection: %+v", body)
			}
		},
		"/fulfillment": func(resp *http.Response) {
			body := decodeReportBody[adminreport.Fulfillment](t, resp)
			if body.Status != "pending" || body.ReservationStatus == "" || body.ReleasePolicy == "" {
				t.Fatalf("unexpected fulfillment projection: %+v", body)
			}
		},
		"/payment-attempts": func(resp *http.Response) {
			body := decodeReportBody[adminreport.AttemptPage](t, resp)
			if len(body.Items) != 1 || body.Summary.TotalAttempts != 1 || body.HasNext {
				t.Fatalf("unexpected attempt page: %+v", body)
			}
		},
		"/payment-attempt-summary": func(resp *http.Response) {
			body := decodeReportBody[adminreport.AttemptSummary](t, resp)
			if body.TotalAttempts != 1 || body.MockSucceededAttempts != 1 || body.RealVerifiedAmountRWF != 0 {
				t.Fatalf("unexpected attempt summary: %+v", body)
			}
		},
	}
	for suffix, check := range subresources {
		resp := e.reportRequest(t, http.MethodGet, "/api/v1/admin/orders/"+order.ID+suffix, nil, true)
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s: %d %s", suffix, resp.StatusCode, body)
		}
		requireNoStore(t, resp)
		check(resp)
	}

	for _, path := range []string{
		"/api/v1/admin/orders/not-a-uuid",
		"/api/v1/admin/orders/00000000-0000-0000-0000-000000000000",
		"/api/v1/admin/orders/00000000-0000-0000-0000-000000000000/items",
	} {
		resp := e.reportRequest(t, http.MethodGet, path, nil, true)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown order path %s: %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	wrongMethod := e.reportRequest(t, http.MethodPost, "/api/v1/admin/orders", map[string]any{}, true)
	if wrongMethod.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("admin orders mutation method accepted: %d", wrongMethod.StatusCode)
	}
	wrongMethod.Body.Close()
}

func TestPhase10DashboardRevenueAccounting(t *testing.T) {
	e := setupAdminReports(t)
	sess := e.registerCustomer(t, "phase10-dashboard-"+phase06ID("cust")+"@example.test", "Customer-Dashboard-Password-123!")
	p := e.createPublishedProduct(t, 70000, 6)

	mockOrder := e.createPaymentOrder(t, sess, p, 1)
	attemptRec := e.initiatePayment(t, sess, mockOrder.ID, phase06ID("dash-mock"), "0781234567")
	attempt := decodePaymentAttempt(t, attemptRec)
	if rec := e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove); rec.Code != http.StatusOK {
		t.Fatalf("mock approve: %d %s", rec.Code, rec.Body.String())
	}

	failedOrder := e.createPaymentOrder(t, sess, p, 1)
	failedAttemptRec := e.initiatePayment(t, sess, failedOrder.ID, phase06ID("dash-fail"), "0781234567")
	failedAttempt := decodePaymentAttempt(t, failedAttemptRec)
	if rec := e.simulatePayment(t, sess, failedAttempt.ID, payment.OutcomeReject); rec.Code != http.StatusOK {
		t.Fatalf("mock reject: %d %s", rec.Code, rec.Body.String())
	}

	verifiedOrder := e.createPaymentOrder(t, sess, p, 1)
	e.insertLiveAttempt(t, verifiedOrder, phase06ID("lv-one"), adminreport.VerificationVerified)
	e.insertLiveAttempt(t, verifiedOrder, phase06ID("lv-two"), adminreport.VerificationVerified)

	unverifiedOrder := e.createPaymentOrder(t, sess, p, 1)
	e.insertLiveAttempt(t, unverifiedOrder, phase06ID("lv-invalid"), "invalid")

	resp := e.reportRequest(t, http.MethodGet, "/api/v1/admin/dashboard?recentLimit=10", nil, true)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dashboard: %d %s", resp.StatusCode, body)
	}
	requireNoStore(t, resp)
	summary := decodeReportBody[adminreport.Dashboard](t, resp)
	if summary.TotalOrders < 4 || summary.PendingPaymentOrders < 2 || summary.MockSuccessfulOrders < 1 ||
		summary.FailedPayments < 1 || summary.FailedPaymentOrders < 1 {
		t.Fatalf("unexpected dashboard counts: %+v", summary)
	}
	if summary.RealVerifiedOrders < 1 || summary.LiveVerifiedAttempts < 2 ||
		summary.RealVerifiedRevenueRWF < verifiedOrder.TotalRWF {
		t.Fatalf("verified revenue was not reported: %+v", summary)
	}
	if summary.RealVerifiedRevenueRWF != verifiedOrder.TotalRWF {
		t.Fatalf("multiple verified attempts double-counted: got %d want %d",
			summary.RealVerifiedRevenueRWF, verifiedOrder.TotalRWF)
	}
	if summary.MockSettledAmountRWF < mockOrder.TotalRWF || summary.MockRevenueIsReal {
		t.Fatalf("mock settlement was misclassified: %+v", summary)
	}
	if !strings.Contains(summary.RevenuePolicy, "Mock settlements are simulations") {
		t.Fatalf("revenue policy is not explicit: %q", summary.RevenuePolicy)
	}
	if summary.FulfillmentCounts[adminreport.FulfillmentPending] < 4 ||
		summary.PaymentStatusCounts[adminreport.PaymentFailed] < 1 {
		t.Fatalf("unexpected state counts: %+v", summary)
	}
	if len(summary.RecentOrders) == 0 || summary.RecentOrders[0].CustomerPhone == sess.Account.PhoneE164 {
		t.Fatalf("recent orders are missing or leak a phone: %+v", summary.RecentOrders)
	}
	invalid := e.reportRequest(t, http.MethodGet, "/api/v1/admin/dashboard?recentLimit=99", nil, true)
	if invalid.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid dashboard query accepted: %d", invalid.StatusCode)
	}
	invalid.Body.Close()
}

func (e *adminReportEnv) insertLiveAttempt(t *testing.T, order orders.Order, key, verification string) {
	t.Helper()
	var customerID, reference string
	var total int64
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT customer_id, reference, total_rwf FROM orders WHERE id=$1`, order.ID).Scan(
		&customerID, &reference, &total); err != nil {
		t.Fatal(err)
	}
	fingerprint := bytes.Repeat([]byte{0x2a}, 32)
	var attemptID string
	err := e.db.Pool().QueryRow(e.ctx, `
		INSERT INTO payment_attempts (
			order_id, customer_id, order_reference, provider_name, provider_environment, is_mock,
			idempotency_key, provider_idempotency_key, request_fingerprint, provider_reference,
			amount_rwf, currency, customer_phone_e164, customer_msisdn, customer_phone_display,
			network, payment_method, state, prompt_status, stock_outcome, completed_at)
		VALUES ($1,$2,$3,'xendlypay','test',false,$4,$5,$6,$7,$8,'RWF',
		        '+250781234567','250781234567','+250 781 234 567','mtn','prompt',
		        'succeeded','not_applicable','reservation_consumed',now())
		RETURNING id`,
		order.ID, customerID, reference, key, "provider-"+key, fingerprint, "LIVE-"+key, total).Scan(&attemptID)
	if err != nil {
		t.Fatalf("insert verified live attempt: %v", err)
	}
	if verification == "" {
		return
	}
	if _, err := e.db.Pool().Exec(e.ctx, `
		INSERT INTO payment_events (
			attempt_id, delivery_id, event_type, provider_name, provider_environment,
			verification_status, payload)
		VALUES ($1,$2,'provider.webhook','xendlypay','test',$3,'{}'::jsonb)`,
		attemptID, "evt-"+key, verification); err != nil {
		t.Fatalf("insert payment event: %v", err)
	}
}

func TestPhase10AdminReportsOverRealTCP(t *testing.T) {
	e := setupAdminReports(t)
	sess := e.registerCustomer(t, "phase10-tcp-"+phase06ID("cust")+"@example.test", "Customer-TCP-Report-Password-123!")
	p := e.createPublishedProduct(t, 60000, 1)
	order := e.createPaymentOrder(t, sess, p, 1)

	srv := server.New(e.cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetAdminReportService(e.reportSvc)
	srv.SetProductService(e.svc)
	srv.SetCustomerService(e.customerSvc)
	srv.SetOrderService(e.orderSvc)
	srv.SetPaymentService(e.paymentSvc)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Phase 10 TCP server did not stop")
		}
	})

	base := "http://" + listener.Addr().String()
	call := func(path string, authenticated bool, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		if authenticated {
			req.AddCookie(e.session)
			req.AddCookie(e.csrf)
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("GET %s: got %d want %d body=%s", path, resp.StatusCode, want, body)
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") {
			t.Fatalf("GET %s missing no-store", path)
		}
		return body
	}

	for _, path := range []string{
		"/api/v1/admin/orders",
		"/api/v1/admin/orders/" + order.ID,
		"/api/v1/admin/dashboard",
	} {
		body := call(path, true, http.StatusOK)
		if !bytes.Contains(body, []byte(order.Reference)) {
			t.Fatalf("TCP %s did not contain the test order", path)
		}
	}
	for _, path := range []string{
		"/api/v1/admin/orders/" + order.ID + "/contact",
		"/api/v1/admin/orders/" + order.ID + "/payment-attempt-summary",
	} {
		call(path, true, http.StatusOK)
	}
	call("/api/v1/admin/orders", false, http.StatusUnauthorized)
}
