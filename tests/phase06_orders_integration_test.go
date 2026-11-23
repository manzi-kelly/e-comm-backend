//go:build !js

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"autivashop/backend/internal/orders"
)

func TestPhase06AuthoritativeOrderCreationAndHistory(t *testing.T) {
	e := setupCustomerOrders(t)
	customerSession := e.registerCustomer(t, "phase06-order-"+phase06ID("cust")+"@example.test", "Customer-Order-Password-123!")
	product := e.createPublishedProduct(t, 125000, 3)
	key := phase06ID("idem")

	if rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(product, 1, customerSession.Account.Email), customerSession, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key: %d %s", rec.Code, rec.Body.String())
	}
	withClientPrice := orderInput(product, 1, customerSession.Account.Email)
	withClientPrice["priceRwf"] = 1
	if rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders", withClientPrice, customerSession, phase06ID("client-price")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("client price accepted: %d %s", rec.Code, rec.Body.String())
	}

	rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(product, 2, customerSession.Account.Email), customerSession, key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create order: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") == "" {
		t.Fatal("missing Location header")
	}
	order := decodeOrder(t, rec)
	if order.Status != orders.FulfillmentPending || order.PaymentStatus != orders.PaymentPending ||
		order.Currency != "RWF" || order.SubtotalRWF != 250000 || order.TotalRWF != 250000 ||
		order.AmountDueRWF != 250000 || order.PaidRWF != 0 || order.PaymentTerm != "full_amount" ||
		order.DeliveryFeeRWF != nil || order.DeliveryFeeStatus != "unavailable" {
		t.Fatalf("unexpected authoritative totals/statuses: %+v", order)
	}
	if !strings.HasPrefix(order.Reference, "AV-"+time.Now().UTC().Format("20060102")+"-") || len(order.Reference) != 24 {
		t.Fatalf("unexpected reference %q", order.Reference)
	}
	if len(order.Items) != 1 || order.Items[0].UnitPriceRWF != 125000 || order.Items[0].LineTotalRWF != 250000 ||
		order.Items[0].Quantity != 2 || order.Items[0].VariantID != nil || order.Items[0].Name.EN == "" {
		t.Fatalf("unexpected item snapshot: %+v", order.Items)
	}
	if len(order.Timeline) != 1 || order.Timeline[0].Step != "placed" || order.Timeline[0].At == nil {
		t.Fatalf("unexpected timeline: %+v", order.Timeline)
	}
	if order.Reservation.Status != "active" || order.Reservation.ExpiresAt == nil {
		t.Fatalf("unexpected reservation: %+v", order.Reservation)
	}

	var dbStatus, paymentStatus, feeStatus string
	var subtotal, fee, total int64
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT fulfillment_status,payment_status,subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf FROM orders WHERE id=$1`, order.ID).
		Scan(&dbStatus, &paymentStatus, &subtotal, &fee, &feeStatus, &total); err != nil {
		t.Fatal(err)
	}
	if dbStatus != "pending" || paymentStatus != "pending_payment" || subtotal != 250000 || fee != 0 || feeStatus != "unavailable" || total != 250000 {
		t.Fatal("authoritative database state mismatch")
	}
	var stock int64
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, product.ID).Scan(&stock); err != nil || stock != 3 {
		t.Fatalf("on-hand stock was deducted: %d %v", stock, err)
	}
	var reserved int64
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT COALESCE(sum(quantity),0) FROM inventory_reservations WHERE product_id=$1 AND order_id=$2 AND status='active'`, product.ID, order.ID).Scan(&reserved); err != nil || reserved != 2 {
		t.Fatalf("reservation missing: %d %v", reserved, err)
	}
	var auditCount int
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM order_audit_events WHERE order_id=$1 AND action='order.created'`, order.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("order audit missing: %d %v", auditCount, err)
	}

	replay := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(product, 2, customerSession.Account.Email), customerSession, key)
	if replay.Code != http.StatusOK || decodeOrder(t, replay).ID != order.ID {
		t.Fatalf("idempotent replay failed: %d %s", replay.Code, replay.Body.String())
	}
	changed := orderInput(product, 1, customerSession.Account.Email)
	conflict := e.customerRequest(t, http.MethodPost, "/api/v1/orders", changed, customerSession, key)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("idempotency key reuse with a different body: %d %s", conflict.Code, conflict.Body.String())
	}

	updated, err := e.svc.Patch(e.ctx, product.ID, product.Version, map[string]json.RawMessage{"priceRwf": json.RawMessage(`999999`)}, e.user.ID)
	if err != nil || updated.PriceRWF != 999999 {
		t.Fatalf("price update failed: %+v %v", updated, err)
	}
	fetched := e.customerRequest(t, http.MethodGet, "/api/v1/orders/"+order.ID, nil, customerSession, "")
	if fetched.Code != http.StatusOK {
		t.Fatalf("get own order: %d %s", fetched.Code, fetched.Body.String())
	}
	if stored := decodeOrder(t, fetched); stored.Items[0].UnitPriceRWF != 125000 || stored.TotalRWF != 250000 {
		t.Fatal("order totals were not immutable snapshots")
	}

	other := e.registerCustomer(t, "phase06-other-"+phase06ID("cust")+"@example.test", "Customer-Other-Password-123!")
	if rec := e.customerRequest(t, http.MethodGet, "/api/v1/orders/"+order.ID, nil, other, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer order read: %d %s", rec.Code, rec.Body.String())
	}
	list := e.customerRequest(t, http.MethodGet, "/api/v1/orders?limit=1", nil, other, "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"items":[]`) {
		t.Fatalf("other customer history leaked: %d %s", list.Code, list.Body.String())
	}
	ownList := e.customerRequest(t, http.MethodGet, "/api/v1/orders?page=1&limit=1", nil, customerSession, "")
	if ownList.Code != http.StatusOK || !strings.Contains(ownList.Body.String(), order.ID) {
		t.Fatalf("owner history missing: %d %s", ownList.Code, ownList.Body.String())
	}
	for _, query := range []string{"page=0", "limit=101", "unknown=1", "page=1&page=2"} {
		if rec := e.customerRequest(t, http.MethodGet, "/api/v1/orders?"+query, nil, customerSession, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid query %q accepted: %d", query, rec.Code)
		}
	}
	if rec := e.customerRequest(t, http.MethodGet, "/api/v1/orders/not-a-uuid", nil, customerSession, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("invalid order id: %d", rec.Code)
	}
}

func TestPhase06PublicationAvailabilityStockAndExpiry(t *testing.T) {
	e := setupCustomerOrders(t)
	sess := e.registerCustomer(t, "phase06-stock-"+phase06ID("cust")+"@example.test", "Customer-Stock-Password-123!")

	unpublished := e.create(t, productInput())
	if rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(unpublished, 1, sess.Account.Email), sess, phase06ID("unpublished")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unpublished product order: %d %s", rec.Code, rec.Body.String())
	}

	p := e.createPublishedProduct(t, 1000, 2)
	first := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(p, 2, sess.Account.Email), sess, phase06ID("consume"))
	if first.Code != http.StatusCreated {
		t.Fatalf("consume stock: %d %s", first.Code, first.Body.String())
	}
	firstOrder := decodeOrder(t, first)
	short := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(p, 1, sess.Account.Email), sess, phase06ID("short"))
	if short.Code != http.StatusConflict {
		t.Fatalf("oversold order accepted: %d %s", short.Code, short.Body.String())
	}

	if _, err := e.db.Exec(e.ctx, `UPDATE inventory_reservations SET created_at=now()-interval '2 seconds', expires_at=now()-interval '1 second' WHERE order_id=$1`, firstOrder.ID); err != nil {
		t.Fatal(err)
	}
	expiredGet := e.customerRequest(t, http.MethodGet, "/api/v1/orders/"+firstOrder.ID, nil, sess, "")
	if expiredGet.Code != http.StatusOK || decodeOrder(t, expiredGet).Reservation.Status != "expired" {
		t.Fatalf("expired reservation not reported: %d %s", expiredGet.Code, expiredGet.Body.String())
	}
	released := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(p, 1, sess.Account.Email), sess, phase06ID("released"))
	if released.Code != http.StatusCreated {
		t.Fatalf("expired reservation did not release stock: %d %s", released.Code, released.Body.String())
	}

	current, err := e.svc.Get(e.ctx, p.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := e.svc.Patch(e.ctx, p.ID, current.Version, map[string]json.RawMessage{
		"availability": json.RawMessage(`"out-of-stock"`), "stockQuantity": json.RawMessage(`0`),
	}, e.user.ID)
	if err != nil || unavailable.Availability != "out-of-stock" {
		t.Fatalf("availability update failed: %+v %v", unavailable, err)
	}
	if rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(p, 1, sess.Account.Email), sess, phase06ID("unavailable")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("out-of-stock product order: %d %s", rec.Code, rec.Body.String())
	}

	archived := e.createPublishedProduct(t, 1000, 1)
	archived, err = e.svc.State(e.ctx, archived.ID, archived.Version, "delete", e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(archived, 1, sess.Account.Email), sess, phase06ID("archived")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("archived product order: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPhase06OrderTransactionRollback(t *testing.T) {
	e := setupCustomerOrders(t)
	sess := e.registerCustomer(t, "phase06-rollback-"+phase06ID("cust")+"@example.test", "Customer-Rollback-Password-123!")
	p := e.createPublishedProduct(t, 1500, 2)
	key := phase06ID("rollback")
	functionName := "phase06_fail_items_" + fmt.Sprint(time.Now().UnixNano())
	triggerName := functionName + "_trg"

	if _, err := e.db.Exec(e.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'forced rollback'; END; $$ LANGUAGE plpgsql`, functionName)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(e.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON order_items FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, functionName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.db.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON order_items`, triggerName))
		_, _ = e.db.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, functionName))
	})

	rec := e.customerRequest(t, http.MethodPost, "/api/v1/orders", orderInput(p, 1, sess.Account.Email), sess, key)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("forced failure: %d %s", rec.Code, rec.Body.String())
	}
	var count int
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM orders WHERE idempotency_key=$1`, key).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial order survived rollback: %d %v", count, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM inventory_reservations WHERE order_id IN (SELECT id FROM orders WHERE idempotency_key=$1)`, key).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial reservation survived rollback: %d %v", count, err)
	}
	var stock int64
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT stock_quantity FROM inventory WHERE product_id=$1`, p.ID).Scan(&stock); err != nil || stock != 2 {
		t.Fatalf("stock changed after rollback: %d %v", stock, err)
	}
}

func TestPhase06ConcurrentOrdersRespectStock(t *testing.T) {
	e := setupCustomerOrders(t)
	first := e.registerCustomer(t, "phase06-conc-a-"+phase06ID("cust")+"@example.test", "Customer-Concurrent-A-Password-123!")
	second := e.registerCustomer(t, "phase06-conc-b-"+phase06ID("cust")+"@example.test", "Customer-Concurrent-B-Password-123!")
	p := e.createPublishedProduct(t, 2000, 1)

	type result struct {
		status int
		body   string
	}
	submit := func(sess *customerTestSession, key string) result {
		body, _ := json.Marshal(orderInput(p, 1, sess.Account.Email))
		req := newOrderRequest(body, sess, key)
		rec := doReq(e.handler, req)
		return result{rec.Code, rec.Body.String()}
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i, sess := range []*customerTestSession{first, second} {
		wg.Add(1)
		go func(s *customerTestSession, index int) {
			defer wg.Done()
			results <- submit(s, phase06ID(fmt.Sprintf("conc-%d", index)))
		}(sess, i)
	}
	wg.Wait()
	close(results)
	created, conflicts := 0, 0
	for r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result %d: %s", r.status, r.body)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("concurrent stock check failed: created=%d conflicts=%d", created, conflicts)
	}
	var orders, reserved int
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM orders o JOIN order_items i ON i.order_id=o.id WHERE i.product_id=$1`, p.ID).Scan(&orders); err != nil || orders != 1 {
		t.Fatalf("expected one order: %d %v", orders, err)
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM inventory_reservations WHERE product_id=$1 AND status='active'`, p.ID).Scan(&reserved); err != nil || reserved != 1 {
		t.Fatalf("expected one reservation: %d %v", reserved, err)
	}
}
