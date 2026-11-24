//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/customer"
	"autivashop/backend/internal/orders"
	"autivashop/backend/internal/product"
	"autivashop/backend/internal/server"
)

type customerOrderEnv struct {
	*productEnv
	cfg             config.Config
	handler         http.Handler
	customerSvc     *customer.Service
	orderSvc        *orders.Service
	customerCookies auth.CookieConfig
}

type customerTestSession struct {
	Account   customer.Profile
	Password  string
	Session   *http.Cookie
	CSRF      *http.Cookie
	CSRFToken string
}

var phase06PhoneCounter atomic.Uint64

func uniqueRwandaPhone() string {
	n := (uint64(time.Now().UnixNano()) + phase06PhoneCounter.Add(1)) % 100_000_000
	return fmt.Sprintf("07%08d", n)
}

func setupCustomerOrders(t *testing.T) *customerOrderEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; Phase 06 integration tests skipped")
	}
	pe := setupProducts(t)
	cfg := testConfig(t, map[string]string{
		"DATABASE_URL":              dsn,
		"CORS_ORIGINS":              "http://localhost:3000,http://127.0.0.1:3000",
		"CORS_ALLOW_CREDENTIALS":    "true",
		"AUTH_COOKIE_SECURE":        "false",
		"AUTH_COOKIE_SAMESITE":      "lax",
		"CUSTOMER_SESSION_TTL":      "30m",
		"ORDER_RESERVATION_TTL":     "30m",
		"AUTH_LOGIN_MAX_FAILURES":   "10",
		"AUTH_LOGIN_LOCKOUT_WINDOW": "2m",
	})
	customerSvc := customer.NewService(pe.db.Pool(), cfg, testLogger())
	orderSvc := orders.NewService(pe.db.Pool(), cfg)
	srv := server.New(cfg, pe.db, testLogger())
	srv.SetAdminService(pe.cfgSvc)
	srv.SetProductService(pe.svc)
	srv.SetCustomerService(customerSvc)
	srv.SetOrderService(orderSvc)
	return &customerOrderEnv{
		productEnv:      pe,
		cfg:             cfg,
		handler:         srv.Handler(),
		customerSvc:     customerSvc,
		orderSvc:        orderSvc,
		customerCookies: auth.NewCustomerCookieConfig(cfg),
	}
}

func (e *customerOrderEnv) cleanupCustomer(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		type cleanupStatement struct {
			sql  string
			args []any
		}
		withOrder := []any{id}
		statements := []cleanupStatement{
			{sql: `SELECT set_config('autivashop.fulfillment_cleanup','on',true)`},
			{sql: `DELETE FROM fulfillment_transitions WHERE order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM order_fulfillment WHERE order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM inventory_consumption_events WHERE order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM payment_audit_events WHERE customer_id=$1 OR order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM payment_transitions WHERE attempt_id IN (SELECT id FROM payment_attempts WHERE customer_id=$1 OR order_id IN (SELECT id FROM orders WHERE customer_id=$1))`, args: withOrder},
			{sql: `DELETE FROM payment_events WHERE attempt_id IN (SELECT id FROM payment_attempts WHERE customer_id=$1 OR order_id IN (SELECT id FROM orders WHERE customer_id=$1))`, args: withOrder},
			{sql: `DELETE FROM payment_attempts WHERE customer_id=$1 OR order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM order_audit_events WHERE customer_id=$1 OR order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM inventory_reservations WHERE order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM order_items WHERE order_id IN (SELECT id FROM orders WHERE customer_id=$1)`, args: withOrder},
			{sql: `DELETE FROM orders WHERE customer_id=$1`, args: withOrder},
			{sql: `DELETE FROM customer_audit_events WHERE customer_id=$1`, args: withOrder},
			{sql: `DELETE FROM customer_sessions WHERE customer_id=$1`, args: withOrder},
			{sql: `DELETE FROM customers WHERE id=$1`, args: withOrder},
		}
		tx, err := e.db.Pool().Begin(ctx)
		if err != nil {
			t.Errorf("customer cleanup begin failed: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
				t.Errorf("customer cleanup failed: %v", err)
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("customer cleanup commit failed: %v", err)
		}
	})
}

func (e *customerOrderEnv) bootstrapCustomerCSRF(t *testing.T) (string, []*http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/customer/auth/me", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := doReq(e.handler, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("customer CSRF bootstrap: got %d want 401 body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.CSRFToken == "" {
		t.Fatalf("missing customer CSRF bootstrap: %s", rec.Body.String())
	}
	return body.CSRFToken, rec.Result().Cookies()
}

func authCookiesFrom(t *testing.T, rec *httptest.ResponseRecorder, sessionName, csrfName string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	var session, csrf *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case sessionName:
			session = c
		case csrfName:
			csrf = c
		}
	}
	if session == nil || csrf == nil {
		t.Fatalf("missing auth cookies in response: %+v", rec.Result().Cookies())
	}
	return session, csrf
}

func (e *customerOrderEnv) registerCustomer(t *testing.T, email, password string) *customerTestSession {
	t.Helper()
	csrfToken, cookies := e.bootstrapCustomerCSRF(t)
	payload := map[string]string{
		"fullName": "Aline Umuhire", "email": email, "phone": uniqueRwandaPhone(),
		"password": password, "confirmPassword": password,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/customer/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", csrfToken)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := doReq(e.handler, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("customer register: got %d want 201 body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Customer  customer.Profile `json:"customer"`
		CSRFToken string           `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.CSRFToken == "" || response.Customer.ID == "" || response.Customer.FullName != "Aline Umuhire" {
		t.Fatalf("unexpected registration response: %s", rec.Body.String())
	}
	sessionCookie, csrfCookie := authCookiesFrom(t, rec, e.customerCookies.SessionName, e.customerCookies.CSRFName)
	e.cleanupCustomer(t, response.Customer.ID)
	return &customerTestSession{
		Account: response.Customer, Password: password, Session: sessionCookie,
		CSRF: csrfCookie, CSRFToken: response.CSRFToken,
	}
}

func (e *customerOrderEnv) customerRequest(t *testing.T, method, path string, body any, sess *customerTestSession, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch value := body.(type) {
	case string:
		raw = []byte(value)
	case nil:
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
	if sess != nil {
		req.AddCookie(sess.Session)
		req.AddCookie(sess.CSRF)
		req.Header.Set("X-CSRF-Token", sess.CSRFToken)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return doReq(e.handler, req)
}

func (e *customerOrderEnv) createPublishedProduct(t *testing.T, price, stock int64) product.Product {
	t.Helper()
	in := productInput()
	in.PriceRWF = price
	in.StockQuantity = stock
	in.LowStockThreshold = 1
	p := e.create(t, in)
	p, err := e.svc.State(e.ctx, p.ID, p.Version, "publish", e.user.ID)
	if err != nil {
		t.Fatalf("publish test product: %v", err)
	}
	return p
}

func phase06ID(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func orderInput(p product.Product, quantity int, email string) map[string]any {
	return map[string]any{
		"fullName": "Aline Umuhire", "email": email, "phone": "0781234567",
		"province": "Kigali", "district": "Gasabo", "sector": "Remera", "cell": "Rukiri",
		"addressLine": "KG 123 St", "instructions": "Leave at reception",
		"items": []map[string]any{{"productId": p.ID, "quantity": quantity}},
	}
}

func newOrderRequest(body []byte, sess *customerTestSession, key string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", sess.CSRFToken)
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(sess.Session)
	req.AddCookie(sess.CSRF)
	return req
}

func decodeOrder(t *testing.T, rec *httptest.ResponseRecorder) orders.Order {
	t.Helper()
	var order orders.Order
	if err := json.Unmarshal(rec.Body.Bytes(), &order); err != nil {
		t.Fatalf("decode order: %v body=%s", err, rec.Body.String())
	}
	return order
}
