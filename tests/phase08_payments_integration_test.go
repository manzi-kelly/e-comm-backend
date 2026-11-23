//go:build !js

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autivashop/backend/internal/config"
	"autivashop/backend/internal/orders"
	"autivashop/backend/internal/payment"
	"autivashop/backend/internal/product"
	"autivashop/backend/internal/server"
)

type paymentEnv struct {
	*customerOrderEnv
	paymentSvc *payment.Service
}

func setupPayments(t *testing.T) *paymentEnv {
	t.Helper()
	e := setupCustomerOrders(t)
	provider := payment.NewMockPaymentProvider(e.cfg.Payment.Environment)
	svc, err := payment.NewService(e.db.Pool(), provider, e.cfg)
	if err != nil {
		t.Fatalf("create payment service: %v", err)
	}
	srv := server.New(e.cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetProductService(e.svc)
	srv.SetCustomerService(e.customerSvc)
	srv.SetOrderService(e.orderSvc)
	srv.SetPaymentService(svc)
	e.handler = srv.Handler()
	return &paymentEnv{customerOrderEnv: e, paymentSvc: svc}
}

func (e *paymentEnv) createPaymentOrder(t *testing.T, sess *customerTestSession, p product.Product, quantity int) orders.Order {
	t.Helper()
	rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders",
		orderInput(p, quantity, sess.Account.Email), sess, phase06ID("payment-order"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create payment order: %d %s", rec.Code, rec.Body.String())
	}
	return decodeOrder(t, rec)
}

func (e *paymentEnv) initiatePayment(t *testing.T, sess *customerTestSession, orderID, key, phone string) *httptest.ResponseRecorder {
	t.Helper()
	return e.customerRequest(t, http.MethodPost, "/api/v1/orders/"+orderID+"/payments",
		map[string]string{"phone": phone}, sess, key)
}

func (e *paymentEnv) simulatePayment(t *testing.T, sess *customerTestSession, attemptID, outcome string) *httptest.ResponseRecorder {
	t.Helper()
	return e.customerRequest(t, http.MethodPost, "/api/v1/dev/payments/"+attemptID+"/simulate",
		map[string]string{"outcome": outcome}, sess, "")
}

func decodePaymentAttempt(t *testing.T, rec *httptest.ResponseRecorder) payment.Attempt {
	t.Helper()
	var attempt payment.Attempt
	if err := json.Unmarshal(rec.Body.Bytes(), &attempt); err != nil {
		t.Fatalf("decode payment attempt: %v body=%s", err, rec.Body.String())
	}
	return attempt
}

func decodeAdminAttempt(t *testing.T, rec *httptest.ResponseRecorder) payment.AdminAttempt {
	t.Helper()
	var attempt payment.AdminAttempt
	if err := json.Unmarshal(rec.Body.Bytes(), &attempt); err != nil {
		t.Fatalf("decode admin payment attempt: %v body=%s", err, rec.Body.String())
	}
	return attempt
}

func (e *paymentEnv) adminPaymentRequest(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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
	req.AddCookie(e.session)
	req.AddCookie(e.csrf)
	req.Header.Set("X-CSRF-Token", e.csrf.Value)
	return doReq(e.handler, req)
}

func TestPhase08InitiateSimulateApproveAndAdminReporting(t *testing.T) {
	e := setupPayments(t)
	sess := e.registerCustomer(t, "phase08-pay-"+phase06ID("cust")+"@example.test", "Customer-Payment-Password-123!")
	other := e.registerCustomer(t, "phase08-other-"+phase06ID("cust")+"@example.test", "Customer-Other-Password-123!")
	p := e.createPublishedProduct(t, 125000, 2)
	order := e.createPaymentOrder(t, sess, p, 1)
	path := "/api/v1/orders/" + order.ID + "/payments"

	if rec := e.customerRequest(t, http.MethodGet, path, nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated payment list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.customerRequest(t, http.MethodPost, path, map[string]string{"phone": "0781234567"}, sess, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.customerRequest(t, http.MethodPost, path, `{"phone":"0781234567","amountRwf":1}`, sess, phase06ID("badbody")); rec.Code != http.StatusBadRequest {
		t.Fatalf("client amount field accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.initiatePayment(t, sess, order.ID, phase06ID("badphone"), "+254781234567"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign phone accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.initiatePayment(t, other, order.ID, phase06ID("notowner"), "0781234567"); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer initiation: %d %s", rec.Code, rec.Body.String())
	}

	key := phase06ID("payment")
	created := e.initiatePayment(t, sess, order.ID, key, "0781234567")
	if created.Code != http.StatusCreated || created.Header().Get("Location") == "" {
		t.Fatalf("initiate payment: %d %s", created.Code, created.Body.String())
	}
	attempt := decodePaymentAttempt(t, created)
	if attempt.State != payment.StatePending || attempt.AmountRWF != 125000 || attempt.Currency != "RWF" ||
		attempt.Provider != "mock" || !attempt.IsMock || attempt.Environment != "test" ||
		attempt.Prompt.Status != payment.PromptSimulatedPending ||
		!strings.HasPrefix(*attempt.ProviderReference, "MOCK-") ||
		attempt.Phone != "+250 781 234 567" || attempt.PhoneE164 != "+250781234567" || attempt.Network != "mtn" {
		t.Fatalf("unexpected pending attempt: %+v", attempt)
	}
	if !strings.Contains(attempt.Prompt.Message, "No phone message") || len(attempt.Transitions) != 1 {
		t.Fatalf("dishonest prompt or missing transition: %+v", attempt)
	}

	replay := e.initiatePayment(t, sess, order.ID, key, "+250 781 234 567")
	if replay.Code != http.StatusOK || decodePaymentAttempt(t, replay).ID != attempt.ID {
		t.Fatalf("idempotent initiate replay failed: %d %s", replay.Code, replay.Body.String())
	}
	if rec := e.initiatePayment(t, sess, order.ID, key, "0781234999"); rec.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict not detected: %d %s", rec.Code, rec.Body.String())
	}

	var stock int64
	var reservationStatus, orderPayment, fulfillment string
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 2 {
		t.Fatalf("pending attempt deducted stock: %d %v", stock, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT status FROM inventory_reservations WHERE order_id=$1`, order.ID).Scan(&reservationStatus); err != nil || reservationStatus != "active" {
		t.Fatalf("pending attempt changed reservation: %q %v", reservationStatus, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT payment_status, fulfillment_status FROM orders WHERE id=$1`, order.ID).Scan(&orderPayment, &fulfillment); err != nil ||
		orderPayment != "payment_pending" || fulfillment != "pending" {
		t.Fatalf("unexpected pending order state: %q %q %v", orderPayment, fulfillment, err)
	}
	for table, want := range map[string]int{
		"payment_events": 1, "payment_transitions": 1, "payment_audit_events": 1,
	} {
		var count int
		if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM `+table+` WHERE attempt_id=$1`, attempt.ID).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count = %d, want %d (%v)", table, count, want, err)
		}
	}

	list := e.customerRequest(t, http.MethodGet, path, nil, sess, "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), attempt.ID) {
		t.Fatalf("owner payment list: %d %s", list.Code, list.Body.String())
	}
	detail := e.customerRequest(t, http.MethodGet, path+"/"+attempt.ID, nil, sess, "")
	if detail.Code != http.StatusOK || decodePaymentAttempt(t, detail).ID != attempt.ID {
		t.Fatalf("owner payment detail: %d %s", detail.Code, detail.Body.String())
	}
	if rec := e.customerRequest(t, http.MethodGet, path+"/"+attempt.ID, nil, other, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer payment detail: %d %s", rec.Code, rec.Body.String())
	}
	otherOrder := e.createPaymentOrder(t, sess, p, 1)
	if rec := e.customerRequest(t, http.MethodGet,
		"/api/v1/orders/"+otherOrder.ID+"/payments/"+attempt.ID, nil, sess, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("attempt was readable under another order: %d %s", rec.Code, rec.Body.String())
	}

	approvedRec := e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove)
	if approvedRec.Code != http.StatusOK {
		t.Fatalf("simulate approve: %d %s", approvedRec.Code, approvedRec.Body.String())
	}
	approved := decodePaymentAttempt(t, approvedRec)
	if approved.State != payment.StateSucceeded || approved.Prompt.Status != payment.PromptSimulatedApproved ||
		approved.StockOutcome != "reservation_consumed" || approved.FailureCode != nil {
		t.Fatalf("unexpected approved attempt: %+v", approved)
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 1 {
		t.Fatalf("approved payment did not deduct stock once: %d %v", stock, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT status FROM inventory_reservations WHERE order_id=$1`, order.ID).Scan(&reservationStatus); err != nil || reservationStatus != "consumed" {
		t.Fatalf("reservation not consumed: %q %v", reservationStatus, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT payment_status, fulfillment_status FROM orders WHERE id=$1`, order.ID).Scan(&orderPayment, &fulfillment); err != nil ||
		orderPayment != "paid" || fulfillment != "pending" {
		t.Fatalf("unexpected approved order state: %q %q %v", orderPayment, fulfillment, err)
	}
	replaySimulation := e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove)
	if replaySimulation.Code != http.StatusOK || decodePaymentAttempt(t, replaySimulation).ID != attempt.ID {
		t.Fatalf("simulation replay failed: %d %s", replaySimulation.Code, replaySimulation.Body.String())
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 1 {
		t.Fatalf("simulation replay double-deducted stock: %d %v", stock, err)
	}
	var transitions int
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT count(*) FROM payment_transitions WHERE attempt_id=$1`, attempt.ID).Scan(&transitions); err != nil || transitions != 2 {
		t.Fatalf("replay created another transition: %d %v", transitions, err)
	}
	if rec := e.initiatePayment(t, sess, order.ID, phase06ID("after-paid"), "0781234567"); rec.Code != http.StatusConflict {
		t.Fatalf("paid order accepted another payment: %d %s", rec.Code, rec.Body.String())
	}

	adminList := e.adminPaymentRequest(t, http.MethodGet,
		"/api/v1/admin/payments?state=succeeded&provider=mock&orderReference="+order.Reference, nil)
	if adminList.Code != http.StatusOK {
		t.Fatalf("admin payment list: %d %s", adminList.Code, adminList.Body.String())
	}
	var listBody struct {
		Items   []payment.AdminAttempt `json:"items"`
		HasNext bool                   `json:"hasNext"`
	}
	if err := json.Unmarshal(adminList.Body.Bytes(), &listBody); err != nil || len(listBody.Items) != 1 || listBody.HasNext {
		t.Fatalf("unexpected admin list: %s %v", adminList.Body.String(), err)
	}
	adminAttempt := listBody.Items[0]
	if adminAttempt.ID != attempt.ID || adminAttempt.AmountRWF != 125000 || !adminAttempt.IsMock ||
		adminAttempt.Provider != "mock" || adminAttempt.OrderReference != order.Reference {
		t.Fatalf("unexpected admin attempt: %+v", adminAttempt)
	}
	if strings.Contains(adminList.Body.String(), "+250781234567") ||
		!strings.Contains(adminAttempt.Phone, "••••") || !strings.Contains(adminAttempt.CustomerPhone, "••••") {
		t.Fatalf("admin list leaked an unmasked phone: %s", adminList.Body.String())
	}
	if adminAttempt.DeliveryLocation.Country != "RW" || adminAttempt.DeliveryLocation.Province != "Kigali" ||
		adminAttempt.DeliveryLocation.District != "Gasabo" || adminAttempt.DeliveryLocation.Sector != "Remera" ||
		adminAttempt.DeliveryLocation.AddressLine != "KG 123 St" {
		t.Fatalf("delivery address was not copied from the order: %+v", adminAttempt.DeliveryLocation)
	}
	adminDetail := e.adminPaymentRequest(t, http.MethodGet, "/api/v1/admin/payments/"+attempt.ID, nil)
	if adminDetail.Code != http.StatusOK {
		t.Fatalf("admin payment detail: %d %s", adminDetail.Code, adminDetail.Body.String())
	}
	detailAttempt := decodeAdminAttempt(t, adminDetail)
	if detailAttempt.ID != attempt.ID || strings.Contains(adminDetail.Body.String(), "+250781234567") {
		t.Fatalf("admin detail leaked an unmasked phone: %s", adminDetail.Body.String())
	}
	summaryRec := e.adminPaymentRequest(t, http.MethodGet, "/api/v1/admin/payments/summary", nil)
	if summaryRec.Code != http.StatusOK {
		t.Fatalf("admin payment summary: %d %s", summaryRec.Code, summaryRec.Body.String())
	}
	var summary payment.Revenue
	if err := json.Unmarshal(summaryRec.Body.Bytes(), &summary); err != nil ||
		summary.MockTotalRWF != 125000 || summary.MockPaidCount != 1 ||
		summary.LiveTotalRWF != 0 || summary.LivePaidCount != 0 || !summary.Separated {
		t.Fatalf("unexpected revenue summary: %s %v", summaryRec.Body.String(), err)
	}

	if rec := e.adminPaymentRequest(t, http.MethodGet, "/api/v1/admin/payments?page=0", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid admin query accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.adminPaymentRequest(t, http.MethodGet,
		"/api/v1/admin/payments/00000000-0000-0000-0000-000000000000", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown admin attempt: %d %s", rec.Code, rec.Body.String())
	}
	unauth := httptest.NewRequest(http.MethodGet, "/api/v1/admin/payments", nil)
	unauth.Header.Set("Origin", "http://localhost:3000")
	if rec := doReq(e.handler, unauth); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin payments: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.customerRequest(t, http.MethodGet, "/api/v1/admin/payments", nil, sess, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("customer principal accepted by admin payments: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPhase08FailedPaymentsReleaseStockAndAllowRetry(t *testing.T) {
	e := setupPayments(t)
	sess := e.registerCustomer(t, "phase08-fail-"+phase06ID("cust")+"@example.test", "Customer-Failure-Password-123!")
	p := e.createPublishedProduct(t, 100000, 1)
	order := e.createPaymentOrder(t, sess, p, 1)

	firstRec := e.initiatePayment(t, sess, order.ID, phase06ID("fail-init"), "0781234567")
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("initiate failed-payment attempt: %d %s", firstRec.Code, firstRec.Body.String())
	}
	first := decodePaymentAttempt(t, firstRec)
	rejectedRec := e.simulatePayment(t, sess, first.ID, payment.OutcomeReject)
	if rejectedRec.Code != http.StatusOK {
		t.Fatalf("simulate reject: %d %s", rejectedRec.Code, rejectedRec.Body.String())
	}
	rejected := decodePaymentAttempt(t, rejectedRec)
	if rejected.State != payment.StateFailed || rejected.FailureCode == nil ||
		*rejected.FailureCode != "SIMULATED_REJECTED" || rejected.StockOutcome != "reservation_released" {
		t.Fatalf("unexpected rejected attempt: %+v", rejected)
	}
	var stock int64
	var reservationStatus, orderPayment, fulfillment string
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 1 {
		t.Fatalf("failed payment deducted stock: %d %v", stock, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT status FROM inventory_reservations WHERE order_id=$1`, order.ID).Scan(&reservationStatus); err != nil || reservationStatus != "released" {
		t.Fatalf("failed payment did not release reservation: %q %v", reservationStatus, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT payment_status, fulfillment_status FROM orders WHERE id=$1`, order.ID).Scan(&orderPayment, &fulfillment); err != nil ||
		orderPayment != "payment_failed" || fulfillment != "pending" {
		t.Fatalf("unexpected failed order state: %q %q %v", orderPayment, fulfillment, err)
	}

	retryRec := e.initiatePayment(t, sess, order.ID, phase06ID("retry-init"), "0781234567")
	if retryRec.Code != http.StatusCreated {
		t.Fatalf("retry after failure was refused: %d %s", retryRec.Code, retryRec.Body.String())
	}
	retry := decodePaymentAttempt(t, retryRec)
	if retry.ID == first.ID {
		t.Fatal("retry reused the failed attempt")
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT status FROM inventory_reservations WHERE order_id=$1`, order.ID).Scan(&reservationStatus); err != nil || reservationStatus != "active" {
		t.Fatalf("retry did not re-reserve stock: %q %v", reservationStatus, err)
	}
	timeoutRec := e.simulatePayment(t, sess, retry.ID, payment.OutcomeTimeout)
	if timeoutRec.Code != http.StatusOK {
		t.Fatalf("simulate timeout: %d %s", timeoutRec.Code, timeoutRec.Body.String())
	}
	timeout := decodePaymentAttempt(t, timeoutRec)
	if timeout.State != payment.StateFailed || timeout.FailureCode == nil ||
		*timeout.FailureCode != "SIMULATED_TIMEOUT" || timeout.StockOutcome != "reservation_released" {
		t.Fatalf("unexpected timeout attempt: %+v", timeout)
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 1 {
		t.Fatalf("timeout deducted stock: %d %v", stock, err)
	}

	finalRec := e.initiatePayment(t, sess, order.ID, phase06ID("final-init"), "0781234567")
	if finalRec.Code != http.StatusCreated {
		t.Fatalf("second retry refused: %d %s", finalRec.Code, finalRec.Body.String())
	}
	final := decodePaymentAttempt(t, finalRec)
	approvedRec := e.simulatePayment(t, sess, final.ID, payment.OutcomeApprove)
	if approvedRec.Code != http.StatusOK {
		t.Fatalf("final approve: %d %s", approvedRec.Code, approvedRec.Body.String())
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 0 {
		t.Fatalf("successful retry did not deduct stock once: %d %v", stock, err)
	}
}

func TestPhase08LateSuccessIsBlockedSafely(t *testing.T) {
	e := setupPayments(t)
	sess := e.registerCustomer(t, "phase08-late-"+phase06ID("cust")+"@example.test", "Customer-Late-Password-123!")
	p := e.createPublishedProduct(t, 90000, 1)
	firstOrder := e.createPaymentOrder(t, sess, p, 1)
	firstRec := e.initiatePayment(t, sess, firstOrder.ID, phase06ID("late-first"), "0781234567")
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("initiate first attempt: %d %s", firstRec.Code, firstRec.Body.String())
	}
	first := decodePaymentAttempt(t, firstRec)

	if _, err := e.db.Exec(e.ctx, `
		UPDATE inventory_reservations SET created_at=now()-interval '2 seconds', expires_at=now()-interval '1 second'
		WHERE order_id=$1`, firstOrder.ID); err != nil {
		t.Fatal(err)
	}
	secondOrder := e.createPaymentOrder(t, sess, p, 1)
	secondRec := e.initiatePayment(t, sess, secondOrder.ID, phase06ID("late-second"), "0781234567")
	if secondRec.Code != http.StatusCreated {
		t.Fatalf("initiate second attempt: %d %s", secondRec.Code, secondRec.Body.String())
	}
	second := decodePaymentAttempt(t, secondRec)
	if approved := e.simulatePayment(t, sess, second.ID, payment.OutcomeApprove); approved.Code != http.StatusOK {
		t.Fatalf("approve second attempt: %d %s", approved.Code, approved.Body.String())
	}

	lateRec := e.simulatePayment(t, sess, first.ID, payment.OutcomeApprove)
	if lateRec.Code != http.StatusOK {
		t.Fatalf("late approve: %d %s", lateRec.Code, lateRec.Body.String())
	}
	late := decodePaymentAttempt(t, lateRec)
	if late.State != payment.StateSucceeded || late.StockOutcome != "late_success_blocked" {
		t.Fatalf("late success was not classified: %+v", late)
	}
	var stock int64
	var paymentStatus, fulfillment string
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 0 {
		t.Fatalf("late success changed stock: %d %v", stock, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT payment_status, fulfillment_status FROM orders WHERE id=$1`, firstOrder.ID).Scan(&paymentStatus, &fulfillment); err != nil ||
		paymentStatus != "paid" || fulfillment != "payment_received_stock_unavailable" {
		t.Fatalf("late success order state: %q %q %v", paymentStatus, fulfillment, err)
	}
	if replay := e.simulatePayment(t, sess, first.ID, payment.OutcomeApprove); replay.Code != http.StatusOK ||
		decodePaymentAttempt(t, replay).StockOutcome != "late_success_blocked" {
		t.Fatalf("late success replay failed: %d %s", replay.Code, replay.Body.String())
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 0 {
		t.Fatalf("late success replay changed stock: %d %v", stock, err)
	}
}

func TestPhase08ProductionRouterHasNoSimulationSurface(t *testing.T) {
	e := setupPayments(t)
	prodCfg := e.cfg
	prodCfg.Environment = config.EnvProduction
	srv := server.New(prodCfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetProductService(e.svc)
	srv.SetCustomerService(e.customerSvc)
	srv.SetOrderService(e.orderSvc)
	srv.SetPaymentService(e.paymentSvc)

	sess := e.registerCustomer(t, "phase08-prod-"+phase06ID("cust")+"@example.test", "Customer-Prod-Password-123!")
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/dev/payments/11111111-1111-1111-1111-111111111111/simulate",
		strings.NewReader(`{"outcome":"approve"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", sess.CSRFToken)
	req.AddCookie(sess.Session)
	req.AddCookie(sess.CSRF)
	if rec := doReq(srv.Handler(), req); rec.Code != http.StatusNotFound {
		t.Fatalf("production exposed the simulation route: %d %s", rec.Code, rec.Body.String())
	}
}
