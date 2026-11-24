//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"autivashop/backend/internal/adminreport"
	"autivashop/backend/internal/fulfillment"
	"autivashop/backend/internal/orders"
	"autivashop/backend/internal/payment"
	"autivashop/backend/internal/product"
	"autivashop/backend/internal/server"

	"github.com/jackc/pgx/v5/pgconn"
)

type fulfillmentEnv struct {
	*adminReportEnv
	fulfillmentSvc *fulfillment.Service
}

func setupFulfillment(t *testing.T) *fulfillmentEnv {
	t.Helper()
	e := setupAdminReports(t)
	svc := fulfillment.NewService(e.db.Pool(), e.cfg)
	srv := server.New(e.cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetAdminReportService(e.reportSvc)
	srv.SetProductService(e.svc)
	srv.SetCustomerService(e.customerSvc)
	srv.SetOrderService(e.orderSvc)
	srv.SetPaymentService(e.paymentSvc)
	srv.SetFulfillmentService(svc)
	e.handler = srv.Handler()
	return &fulfillmentEnv{adminReportEnv: e, fulfillmentSvc: svc}
}

func (e *fulfillmentEnv) adminFulfillment(t *testing.T, method, path string, body any, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch value := body.(type) {
	case nil:
	case string:
		raw = []byte(value)
	default:
		var err error
		raw, err = json.Marshal(value)
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
	return doReq(e.handler, req)
}

func decodeFulfillment[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode fulfillment response: %v body=%s", err, rec.Body.String())
	}
	return value
}

func (e *fulfillmentEnv) makePaidOrder(t *testing.T, sess *customerTestSession, p product.Product, quantity int) orders.Order {
	t.Helper()
	order := e.createPaymentOrder(t, sess, p, quantity)
	attemptRec := e.initiatePayment(t, sess, order.ID, phase06ID("fulfill-pay"), "0781234567")
	if attemptRec.Code != http.StatusCreated {
		t.Fatalf("initiate payment: %d %s", attemptRec.Code, attemptRec.Body.String())
	}
	attempt := decodePaymentAttempt(t, attemptRec)
	approved := e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove)
	if approved.Code != http.StatusOK {
		t.Fatalf("approve payment: %d %s", approved.Code, approved.Body.String())
	}
	return order
}

func fulfillmentKey(prefix, orderID string) string {
	return prefix + "-" + orderID
}

func assertConflictContains(t *testing.T, conflicts []fulfillment.Conflict, code string) {
	t.Helper()
	for _, conflict := range conflicts {
		if conflict.Code == code {
			return
		}
	}
	t.Fatalf("conflict %q missing from %+v", code, conflicts)
}

func expectPgCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected PostgreSQL error %s, got nil", code)
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != code {
		t.Fatalf("expected PostgreSQL error %s, got %v", code, err)
	}
}

func TestPhase12ForwardStateMachinePaymentEligibilityAndAudit(t *testing.T) {
	e := setupFulfillment(t)
	sess := e.registerCustomer(t, "phase12-state-"+phase06ID("cust")+"@example.test", "Customer-State-Password-123!")
	other := e.registerCustomer(t, "phase12-other-"+phase06ID("cust")+"@example.test", "Customer-Other-Password-123!")
	p := e.createPublishedProduct(t, 100000, 2)
	order := e.createPaymentOrder(t, sess, p, 1)

	base := "/api/v1/admin/orders/" + order.ID + "/fulfillment"
	stateRec := e.adminFulfillment(t, http.MethodGet, base+"/state", nil, true)
	if stateRec.Code != http.StatusOK {
		t.Fatalf("admin state read: %d %s", stateRec.Code, stateRec.Body.String())
	}
	state := decodeFulfillment[fulfillment.State](t, stateRec)
	if state.Status != fulfillment.StatusPending || state.Version != 0 ||
		state.SettlementMode != fulfillment.SettlementNotPaid || state.LocalDelivery.Country != "RW" ||
		state.LocalDelivery.Province != "Kigali" || state.LocalDelivery.District != "Gasabo" {
		t.Fatalf("unexpected initial state: %+v", state)
	}
	if strings.Contains(stateRec.Body.String(), sess.Account.PhoneE164) ||
		!strings.Contains(state.LocalDelivery.PhoneMasked, "••••") {
		t.Fatalf("state leaked an unmasked phone: %s", stateRec.Body.String())
	}

	conflictRec := e.adminFulfillment(t, http.MethodGet, base+"/conflicts", nil, true)
	if conflictRec.Code != http.StatusOK {
		t.Fatalf("admin conflicts read: %d %s", conflictRec.Code, conflictRec.Body.String())
	}
	conflicts := decodeFulfillment[struct {
		Items []fulfillment.Conflict `json:"items"`
	}](t, conflictRec).Items
	assertConflictContains(t, conflicts, "payment.not_paid")

	customerPath := "/api/v1/orders/" + order.ID + "/fulfillment"
	if rec := e.customerRequest(t, http.MethodGet, customerPath, nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated customer fulfillment read: %d %s", rec.Code, rec.Body.String())
	}
	ownerRec := e.customerRequest(t, http.MethodGet, customerPath, nil, sess, "")
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("owner fulfillment read: %d %s", ownerRec.Code, ownerRec.Body.String())
	}
	ownerView := decodeFulfillment[fulfillment.CustomerView](t, ownerRec)
	if ownerView.State.Status != fulfillment.StatusPending || len(ownerView.History) != 0 ||
		strings.Contains(ownerRec.Body.String(), e.user.Email) {
		t.Fatalf("unexpected customer view: %+v", ownerView)
	}
	if rec := e.customerRequest(t, http.MethodGet, customerPath, nil, other, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer fulfillment read: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.customerRequest(t, http.MethodPost, base+"/processing", map[string]any{}, sess, ""); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("customer principal reached admin mutation: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 0, "idempotencyKey": fulfillmentKey("anon", order.ID)}, false); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated admin transition: %d %s", rec.Code, rec.Body.String())
	}

	// Strict body validation rejects unknown fields, missing keys and bad media types.
	badBody := `{"expectedVersion":0,"idempotencyKey":"phase12-valid-key","status":"delivered"}`
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/processing", badBody, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 0}, true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing idempotency key accepted: %d %s", rec.Code, rec.Body.String())
	}
	wrongType := httptest.NewRequest(http.MethodPost, base+"/processing", bytes.NewReader([]byte(`{}`)))
	wrongType.Header.Set("Content-Type", "text/plain")
	wrongType.Header.Set("Origin", "http://localhost:3000")
	wrongType.AddCookie(e.session)
	wrongType.AddCookie(e.csrf)
	wrongType.Header.Set("X-CSRF-Token", e.csrf.Value)
	if rec := doReq(e.handler, wrongType); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type accepted: %d %s", rec.Code, rec.Body.String())
	}

	// Payment is not authoritative until settlement is recorded by the backend.
	key := fulfillmentKey("processing", order.ID)
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 0, "idempotencyKey": key}, true); rec.Code != http.StatusConflict {
		t.Fatalf("unpaid order entered processing: %d %s", rec.Code, rec.Body.String())
	}

	attemptRec := e.initiatePayment(t, sess, order.ID, phase06ID("state-pay"), "0781234567")
	if attemptRec.Code != http.StatusCreated {
		t.Fatalf("initiate payment: %d %s", attemptRec.Code, attemptRec.Body.String())
	}
	attempt := decodePaymentAttempt(t, attemptRec)
	if approved := e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove); approved.Code != http.StatusOK {
		t.Fatalf("approve payment: %d %s", approved.Code, approved.Body.String())
	}
	var consumed int
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT count(*) FROM inventory_consumption_events WHERE order_id=$1`, order.ID).Scan(&consumed); err != nil || consumed != 1 {
		t.Fatalf("consumption record missing: %d %v", consumed, err)
	}

	if rec := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 1, "idempotencyKey": key}, true); rec.Code != http.StatusConflict {
		t.Fatalf("stale version accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/ready-for-delivery",
		map[string]any{"expectedVersion": 0, "idempotencyKey": fulfillmentKey("skip", order.ID)}, true); rec.Code != http.StatusConflict {
		t.Fatalf("state skipping accepted: %d %s", rec.Code, rec.Body.String())
	}

	processing := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 0, "idempotencyKey": key, "note": "Packed and verified"}, true)
	if processing.Code != http.StatusOK {
		t.Fatalf("start processing: %d %s", processing.Code, processing.Body.String())
	}
	processingResult := decodeFulfillment[fulfillment.TransitionResult](t, processing)
	if processingResult.Replayed || processingResult.State.Status != fulfillment.StatusProcessing ||
		processingResult.State.Version != 1 || processingResult.State.SettlementMode != fulfillment.SettlementSimulated {
		t.Fatalf("unexpected processing result: %+v", processingResult)
	}
	replay := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 0, "idempotencyKey": key, "note": "Packed and verified"}, true)
	if replay.Code != http.StatusAccepted || decodeFulfillment[fulfillment.TransitionResult](t, replay).Replayed != true {
		t.Fatalf("idempotent replay failed: %d %s", replay.Code, replay.Body.String())
	}
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 1, "idempotencyKey": key}, true); rec.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict not detected: %d %s", rec.Code, rec.Body.String())
	}

	steps := []struct {
		path    string
		version int64
		note    string
		want    string
	}{
		{"/ready-for-delivery", 1, "Label printed", fulfillment.StatusReadyForDelivery},
		{"/out-for-delivery", 2, "Handed to local rider", fulfillment.StatusOutForDelivery},
		{"/delivered", 3, "Recipient confirmed by phone", fulfillment.StatusDelivered},
	}
	for _, step := range steps {
		rec := e.adminFulfillment(t, http.MethodPost, base+step.path, map[string]any{
			"expectedVersion": step.version,
			"idempotencyKey":  fulfillmentKey(strings.Trim(step.path, "/"), order.ID),
			"note":            step.note,
		}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("transition %s: %d %s", step.path, rec.Code, rec.Body.String())
		}
		result := decodeFulfillment[fulfillment.TransitionResult](t, rec)
		if result.State.Status != step.want || result.State.Version != step.version+1 {
			t.Fatalf("unexpected %s result: %+v", step.path, result)
		}
	}
	finalState := decodeFulfillment[fulfillment.State](t, e.adminFulfillment(t, http.MethodGet, base+"/state", nil, true))
	if finalState.Status != fulfillment.StatusDelivered || finalState.Confirmation == nil ||
		finalState.Confirmation.Method != fulfillment.ConfirmationMethod ||
		finalState.Confirmation.Source != fulfillment.ConfirmationSource ||
		finalState.Confirmation.Note == nil || *finalState.Confirmation.Note != "Recipient confirmed by phone" {
		t.Fatalf("delivery confirmation missing: %+v", finalState)
	}
	var orderStatus string
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT fulfillment_status FROM orders WHERE id=$1`, order.ID).Scan(&orderStatus); err != nil || orderStatus != fulfillment.StatusDelivered {
		t.Fatalf("order projection not synced: %q %v", orderStatus, err)
	}

	historyRec := e.adminFulfillment(t, http.MethodGet, base+"/history", nil, true)
	if historyRec.Code != http.StatusOK {
		t.Fatalf("admin history: %d %s", historyRec.Code, historyRec.Body.String())
	}
	history := decodeFulfillment[struct {
		Items []fulfillment.AdminTransition `json:"items"`
	}](t, historyRec).Items
	if len(history) != 4 || history[0].ToStatus != fulfillment.StatusDelivered || history[0].ActorEmail == nil ||
		*history[0].ActorEmail != e.user.Email || history[0].ActorAdminID == nil {
		t.Fatalf("unexpected admin history: %+v", history)
	}
	customerView := decodeFulfillment[fulfillment.CustomerView](t,
		e.customerRequest(t, http.MethodGet, customerPath, nil, sess, ""))
	if len(customerView.History) != 4 || strings.Contains(customerView.History[0].EventType, "note") {
		t.Fatalf("unexpected customer history: %+v", customerView.History)
	}
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/delivered",
		map[string]any{"expectedVersion": 4, "idempotencyKey": fulfillmentKey("after-delivered", order.ID)}, true); rec.Code != http.StatusConflict {
		t.Fatalf("transition after delivery accepted: %d %s", rec.Code, rec.Body.String())
	}

	listResp := e.reportRequest(t, http.MethodGet,
		"/api/v1/admin/orders?fulfillmentStatus=delivered&search="+order.Reference, nil, true)
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("admin fulfillment filter: %d %s", listResp.StatusCode, body)
	}
	page := decodeReportBody[adminreport.OrderPage](t, listResp)
	found := false
	for _, item := range page.Items {
		if item.ID == order.ID && item.FulfillmentStatus == fulfillment.StatusDelivered {
			found = true
		}
	}
	if !found {
		t.Fatal("admin list did not expose delivered fulfillment")
	}
}

func TestPhase12ReservationAndAllocationSafety(t *testing.T) {
	e := setupFulfillment(t)
	sess := e.registerCustomer(t, "phase12-stock-"+phase06ID("cust")+"@example.test", "Customer-Stock-Password-123!")
	p := e.createPublishedProduct(t, 50000, 1)
	order := e.createPaymentOrder(t, sess, p, 1)

	// Limited stock cannot be reserved twice.
	if rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders",
		orderInput(p, 1, sess.Account.Email), sess, phase06ID("oversell")); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusConflict {
		t.Fatalf("oversell accepted: %d %s", rec.Code, rec.Body.String())
	}

	attemptRec := e.initiatePayment(t, sess, order.ID, phase06ID("expire-pay"), "0781234567")
	if attemptRec.Code != http.StatusCreated {
		t.Fatalf("initiate payment: %d %s", attemptRec.Code, attemptRec.Body.String())
	}
	attempt := decodePaymentAttempt(t, attemptRec)
	if _, err := e.db.Pool().Exec(e.ctx, `
		UPDATE inventory_reservations
		SET created_at=now() - interval '2 hours', expires_at=now() - interval '1 hour'
		WHERE order_id=$1`, order.ID); err != nil {
		t.Fatalf("expire reservation: %v", err)
	}
	if approved := e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove); approved.Code != http.StatusOK {
		t.Fatalf("late success: %d %s", approved.Code, approved.Body.String())
	}
	var legacyStatus string
	var stock int64
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT fulfillment_status FROM orders WHERE id=$1`, order.ID).Scan(&legacyStatus); err != nil {
		t.Fatal(err)
	}
	if legacyStatus != fulfillment.LegacyStockUnavailable {
		t.Fatalf("late success was not blocked: %q", legacyStatus)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 1 {
		t.Fatalf("expired reservation deducted stock: %d %v", stock, err)
	}
	base := "/api/v1/admin/orders/" + order.ID + "/fulfillment"
	conflictsRec := e.adminFulfillment(t, http.MethodGet, base+"/conflicts", nil, true)
	conflicts := decodeFulfillment[struct {
		Items []fulfillment.Conflict `json:"items"`
	}](t, conflictsRec).Items
	assertConflictContains(t, conflicts, "inventory.stock_unavailable")
	if rec := e.adminFulfillment(t, http.MethodPost, base+"/processing",
		map[string]any{"expectedVersion": 0, "idempotencyKey": fulfillmentKey("blocked", order.ID)}, true); rec.Code != http.StatusConflict {
		t.Fatalf("stock-blocked order entered processing: %d %s", rec.Code, rec.Body.String())
	}
	var historyCount int
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT count(*) FROM fulfillment_transitions WHERE order_id=$1`, order.ID).Scan(&historyCount); err != nil || historyCount != 0 {
		t.Fatalf("blocked transition wrote history: %d %v", historyCount, err)
	}

	// A missing consumption record is an unresolved allocation, never a reason
	// to repair stock silently.
	p2 := e.createPublishedProduct(t, 60000, 2)
	order2 := e.makePaidOrder(t, sess, p2, 1)
	cleanupTx, err := e.db.Pool().Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupTx.Rollback(e.ctx)
	if _, err := cleanupTx.Exec(e.ctx, `SELECT set_config('autivashop.fulfillment_cleanup','on',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupTx.Exec(e.ctx,
		`DELETE FROM inventory_consumption_events WHERE order_id=$1`, order2.ID); err != nil {
		t.Fatalf("remove consumption record: %v", err)
	}
	if err := cleanupTx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	base2 := "/api/v1/admin/orders/" + order2.ID + "/fulfillment"
	conflicts2 := decodeFulfillment[struct {
		Items []fulfillment.Conflict `json:"items"`
	}](t, e.adminFulfillment(t, http.MethodGet, base2+"/conflicts", nil, true)).Items
	assertConflictContains(t, conflicts2, "inventory.allocation_unresolved")
	if rec := e.adminFulfillment(t, http.MethodPost, base2+"/processing",
		map[string]any{"expectedVersion": 0, "idempotencyKey": fulfillmentKey("unresolved", order2.ID)}, true); rec.Code != http.StatusConflict {
		t.Fatalf("unresolved allocation entered processing: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPhase12ConcurrentTransitionsAndPayment(t *testing.T) {
	e := setupFulfillment(t)
	sess := e.registerCustomer(t, "phase12-concurrent-"+phase06ID("cust")+"@example.test", "Customer-Concurrent-Password-123!")
	p := e.createPublishedProduct(t, 70000, 4)

	order := e.makePaidOrder(t, sess, p, 1)
	base := "/api/v1/admin/orders/" + order.ID + "/fulfillment/processing"
	const workers = 8
	var wg sync.WaitGroup
	codes := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			rec := e.adminFulfillment(t, http.MethodPost, base, map[string]any{
				"expectedVersion": 0,
				"idempotencyKey":  phase06ID("parallel-" + string(rune('a'+index))),
			}, true)
			codes[index] = rec.Code
		}(i)
	}
	wg.Wait()
	successes := 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected concurrent transition status %d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent transitions produced %d successes, want 1", successes)
	}
	var version int64
	var historyCount int
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT version FROM order_fulfillment WHERE order_id=$1`, order.ID).Scan(&version); err != nil || version != 1 {
		t.Fatalf("unexpected version after concurrency: %d %v", version, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT count(*) FROM fulfillment_transitions WHERE order_id=$1`, order.ID).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("duplicate audit rows after concurrency: %d %v", historyCount, err)
	}

	// A settlement racing a transition cannot advance fulfillment before the
	// backend observes authoritative paid state.
	order2 := e.createPaymentOrder(t, sess, p, 1)
	attemptRec := e.initiatePayment(t, sess, order2.ID, phase06ID("race-pay"), "0781234567")
	if attemptRec.Code != http.StatusCreated {
		t.Fatalf("initiate racing payment: %d %s", attemptRec.Code, attemptRec.Body.String())
	}
	attempt := decodePaymentAttempt(t, attemptRec)
	var raceWG sync.WaitGroup
	var paymentCode, transitionCode int
	raceWG.Add(2)
	go func() {
		defer raceWG.Done()
		paymentCode = e.simulatePayment(t, sess, attempt.ID, payment.OutcomeApprove).Code
	}()
	go func() {
		defer raceWG.Done()
		transitionCode = e.adminFulfillment(t, http.MethodPost,
			"/api/v1/admin/orders/"+order2.ID+"/fulfillment/processing",
			map[string]any{"expectedVersion": 0, "idempotencyKey": fulfillmentKey("race", order2.ID)}, true).Code
	}()
	raceWG.Wait()
	if paymentCode != http.StatusOK {
		t.Fatalf("racing payment failed: %d", paymentCode)
	}
	if transitionCode == http.StatusConflict {
		retry := e.adminFulfillment(t, http.MethodPost,
			"/api/v1/admin/orders/"+order2.ID+"/fulfillment/processing",
			map[string]any{"expectedVersion": 0, "idempotencyKey": fulfillmentKey("race-retry", order2.ID)}, true)
		if retry.Code != http.StatusOK {
			t.Fatalf("post-payment transition retry: %d %s", retry.Code, retry.Body.String())
		}
		transitionCode = retry.Code
	}
	if transitionCode != http.StatusOK {
		t.Fatalf("racing transition failed: %d", transitionCode)
	}
	var paymentStatus, fulfillmentStatus string
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT payment_status, fulfillment_status FROM orders WHERE id=$1`, order2.ID).Scan(&paymentStatus, &fulfillmentStatus); err != nil {
		t.Fatal(err)
	}
	if paymentStatus != "paid" || fulfillmentStatus != fulfillment.StatusProcessing {
		t.Fatalf("payment/fulfillment race lost consistency: %q %q", paymentStatus, fulfillmentStatus)
	}
}

func TestPhase12SQLGuardsAndAuditImmutability(t *testing.T) {
	e := setupFulfillment(t)
	sess := e.registerCustomer(t, "phase12-sql-"+phase06ID("cust")+"@example.test", "Customer-SQL-Password-123!")
	p := e.createPublishedProduct(t, 45000, 2)
	order := e.makePaidOrder(t, sess, p, 1)

	_, err := e.db.Pool().Exec(e.ctx, `
		UPDATE order_fulfillment SET status='delivered', version=1 WHERE order_id=$1`, order.ID)
	expectPgCode(t, err, "23514")

	_, err = e.db.Pool().Exec(e.ctx, `
		INSERT INTO fulfillment_transitions (
			order_id, from_status, to_status, version, event_type, actor_type,
			idempotency_key, request_fingerprint)
		VALUES ($1,'pending','delivered',1,'fulfillment.delivered.confirmed','system',
		        'invalid-transition-key', $2)`, order.ID, make([]byte, 32))
	expectPgCode(t, err, "23514")

	var reservationID, productID string
	var quantity int64
	if err := e.db.Pool().QueryRow(e.ctx, `
		SELECT r.id, r.product_id, r.quantity
		FROM inventory_reservations r WHERE r.order_id=$1`, order.ID).Scan(&reservationID, &productID, &quantity); err != nil {
		t.Fatalf("read consumption source: %v", err)
	}
	_, err = e.db.Pool().Exec(e.ctx, `
		INSERT INTO inventory_consumption_events (order_id, product_id, reservation_id, quantity, source)
		VALUES ($1,$2,$3,$4,'payment_simulation')`, order.ID, productID, reservationID, quantity)
	if err == nil {
		t.Fatal("duplicate consumption record accepted")
	}

	process := e.adminFulfillment(t, http.MethodPost,
		"/api/v1/admin/orders/"+order.ID+"/fulfillment/processing",
		map[string]any{"expectedVersion": 0, "idempotencyKey": fulfillmentKey("sql", order.ID)}, true)
	if process.Code != http.StatusOK {
		t.Fatalf("start processing: %d %s", process.Code, process.Body.String())
	}
	_, err = e.db.Pool().Exec(e.ctx, `
		DELETE FROM fulfillment_transitions WHERE order_id=$1`, order.ID)
	expectPgCode(t, err, "23514")
	_, err = e.db.Pool().Exec(e.ctx, `
		UPDATE fulfillment_transitions SET note='changed' WHERE order_id=$1`, order.ID)
	expectPgCode(t, err, "23514")
}

func TestPhase12FulfillmentOverRealTCP(t *testing.T) {
	e := setupFulfillment(t)
	sess := e.registerCustomer(t, "phase12-tcp-"+phase06ID("cust")+"@example.test", "Customer-TCP-Fulfillment-Password-123!")
	p := e.createPublishedProduct(t, 55000, 1)
	order := e.makePaidOrder(t, sess, p, 1)

	srv := server.New(e.cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetAdminReportService(e.reportSvc)
	srv.SetProductService(e.svc)
	srv.SetCustomerService(e.customerSvc)
	srv.SetOrderService(e.orderSvc)
	srv.SetPaymentService(e.paymentSvc)
	srv.SetFulfillmentService(e.fulfillmentSvc)
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
			t.Error("Phase 12 TCP server did not stop")
		}
	})

	baseURL := "http://" + listener.Addr().String()
	call := func(method, path string, body any, admin, customer bool, want int) []byte {
		t.Helper()
		var raw []byte
		if body != nil {
			var err error
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if admin {
			req.AddCookie(e.session)
			req.AddCookie(e.csrf)
			req.Header.Set("X-CSRF-Token", e.csrf.Value)
		}
		if customer {
			req.AddCookie(sess.Session)
			req.AddCookie(sess.CSRF)
			req.Header.Set("X-CSRF-Token", sess.CSRFToken)
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d body=%s", method, path, resp.StatusCode, want, payload)
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") {
			t.Fatalf("%s %s missing no-store", method, path)
		}
		return payload
	}

	adminBase := "/api/v1/admin/orders/" + order.ID + "/fulfillment"
	customerPath := "/api/v1/orders/" + order.ID + "/fulfillment"
	call(http.MethodGet, adminBase+"/state", nil, true, false, http.StatusOK)
	call(http.MethodGet, customerPath, nil, false, true, http.StatusOK)
	call(http.MethodGet, adminBase+"/conflicts", nil, true, false, http.StatusOK)
	call(http.MethodPost, adminBase+"/processing", map[string]any{
		"expectedVersion": 0, "idempotencyKey": fulfillmentKey("tcp", order.ID),
	}, true, false, http.StatusOK)
	call(http.MethodGet, adminBase+"/history", nil, true, false, http.StatusOK)
	call(http.MethodGet, adminBase+"/state", nil, false, false, http.StatusUnauthorized)
}
