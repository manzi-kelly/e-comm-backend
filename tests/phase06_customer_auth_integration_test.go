//go:build !js

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/customer"
)

func (e *customerOrderEnv) loginCustomer(t *testing.T, email, password string, csrfToken string, csrfCookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/customer/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.AddCookie(csrfCookie)
	return doReq(e.handler, req)
}

func TestPhase06CustomerAuthenticationLifecycle(t *testing.T) {
	e := setupCustomerOrders(t)
	email := "phase06-life-" + phase06ID("cust") + "@example.test"
	password := "Customer-Lifecycle-Password-123!"
	first := e.registerCustomer(t, email, password)

	if !first.Session.HttpOnly || first.CSRF.HttpOnly {
		t.Fatal("customer session cookie must be HttpOnly and CSRF cookie must be readable")
	}
	if first.Session.SameSite != http.SameSiteLaxMode || first.Session.Name != e.customerCookies.SessionName ||
		first.CSRF.Name != e.customerCookies.CSRFName {
		t.Fatalf("unexpected customer cookie attributes: %+v %+v", first.Session, first.CSRF)
	}

	var hash string
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT password_hash FROM customers WHERE id=$1`, first.Account.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, password) {
		t.Fatal("customer password was not safely hashed")
	}
	var tokenHash []byte
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT token_hash FROM customer_sessions WHERE customer_id=$1 ORDER BY created_at DESC LIMIT 1`, first.Account.ID).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if len(tokenHash) != 32 || bytes.Equal(tokenHash, []byte(first.Session.Value)) {
		t.Fatal("raw customer session token must not be stored")
	}
	var audits int
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM customer_audit_events WHERE customer_id=$1 AND action='customer.registered'`, first.Account.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("registration audit missing: %d %v", audits, err)
	}

	me := e.customerRequest(t, http.MethodGet, "/api/v1/customer/auth/me", nil, first, "")
	if me.Code != http.StatusOK {
		t.Fatalf("me: %d %s", me.Code, me.Body.String())
	}
	var meBody struct {
		Customer customer.Profile `json:"customer"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &meBody); err != nil || meBody.Customer.ID != first.Account.ID || meBody.Customer.FullName != "Aline Umuhire" {
		t.Fatalf("unexpected me response: %s", me.Body.String())
	}

	wrong := e.loginCustomer(t, email, "Wrong-Password-12345!", first.CSRFToken, first.CSRF)
	if wrong.Code != http.StatusUnauthorized || !strings.Contains(wrong.Body.String(), "Invalid email or password") {
		t.Fatalf("wrong password: %d %s", wrong.Code, wrong.Body.String())
	}
	if strings.Contains(strings.ToLower(wrong.Body.String()), "disabled") || strings.Contains(strings.ToLower(wrong.Body.String()), "not found") {
		t.Fatalf("credential failure leaked account state: %s", wrong.Body.String())
	}

	good := e.loginCustomer(t, email, password, first.CSRFToken, first.CSRF)
	if good.Code != http.StatusOK {
		t.Fatalf("login: %d %s", good.Code, good.Body.String())
	}
	var loginBody struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(good.Body.Bytes(), &loginBody); err != nil || loginBody.CSRFToken == "" {
		t.Fatalf("missing login CSRF token: %s", good.Body.String())
	}
	secondSession, secondCSRF := authCookiesFrom(t, good, e.customerCookies.SessionName, e.customerCookies.CSRFName)
	if secondSession.Value == first.Session.Value {
		t.Fatal("login must rotate the customer session token")
	}
	second := &customerTestSession{Account: first.Account, Password: password, Session: secondSession, CSRF: secondCSRF, CSRFToken: loginBody.CSRFToken}

	logout := e.customerRequest(t, http.MethodPost, "/api/v1/customer/auth/logout", nil, second, "")
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", logout.Code, logout.Body.String())
	}
	if me := e.customerRequest(t, http.MethodGet, "/api/v1/customer/auth/me", nil, second, ""); me.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session still valid: %d", me.Code)
	}
	if me := e.customerRequest(t, http.MethodGet, "/api/v1/customer/auth/me", nil, first, ""); me.Code != http.StatusOK {
		t.Fatalf("logout revoked a different session: %d", me.Code)
	}

	if _, err := e.db.Exec(e.ctx, `UPDATE customer_sessions SET created_at=now()-interval '2 minutes', expires_at=now()-interval '1 minute' WHERE customer_id=$1 AND token_hash=$2`, first.Account.ID, auth.HashToken(first.Session.Value)); err != nil {
		t.Fatal(err)
	}
	if me := e.customerRequest(t, http.MethodGet, "/api/v1/customer/auth/me", nil, first, ""); me.Code != http.StatusUnauthorized {
		t.Fatalf("expired session still valid: %d", me.Code)
	}
	if _, err := e.db.Exec(e.ctx, `UPDATE customers SET status='disabled' WHERE id=$1`, first.Account.ID); err != nil {
		t.Fatal(err)
	}
	disabled := e.loginCustomer(t, email, password, first.CSRFToken, first.CSRF)
	if disabled.Code != http.StatusUnauthorized || strings.Contains(strings.ToLower(disabled.Body.String()), "disabled") {
		t.Fatalf("disabled account login leaked state: %d %s", disabled.Code, disabled.Body.String())
	}
}

func TestPhase06CustomerThrottling(t *testing.T) {
	e := setupCustomerOrders(t)
	email := "phase06-limit-" + phase06ID("cust") + "@example.test"
	password := "Customer-Throttle-Password-123!"
	sess := e.registerCustomer(t, email, password)
	e.customerSvc.SetLimiter(auth.NewLoginLimiter(2, 2*time.Minute))

	for i := 0; i < 2; i++ {
		rec := e.loginCustomer(t, email, "Wrong-Password-12345!", sess.CSRFToken, sess.CSRF)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	blocked := e.loginCustomer(t, email, password, sess.CSRFToken, sess.CSRF)
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") == "" {
		t.Fatalf("customer login was not throttled: %d %s", blocked.Code, blocked.Body.String())
	}
}

func TestPhase06CustomerCSRFOriginAndAdminIsolation(t *testing.T) {
	e := setupCustomerOrders(t)
	sess := e.registerCustomer(t, "phase06-isolation-"+phase06ID("cust")+"@example.test", "Customer-Isolation-Password-123!")

	// Rebuild without the CSRF header while retaining cookies.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/customer/auth/logout", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.AddCookie(sess.Session)
	req.AddCookie(sess.CSRF)
	rec := doReq(e.handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing customer CSRF header accepted: %d", rec.Code)
	}

	badOriginReq := httptest.NewRequest(http.MethodPost, "/api/v1/customer/auth/login", bytes.NewReader([]byte(`{"email":"x@example.test","password":"y"}`)))
	badOriginReq.Header.Set("Content-Type", "application/json")
	badOriginReq.Header.Set("Origin", "https://evil.example")
	badOriginReq.Header.Set("X-CSRF-Token", sess.CSRFToken)
	badOriginReq.AddCookie(sess.CSRF)
	if rec := doReq(e.handler, badOriginReq); rec.Code != http.StatusForbidden {
		t.Fatalf("disallowed origin accepted: %d", rec.Code)
	}

	p := e.createPublishedProduct(t, 1000, 1)
	adminCSRFReq := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(mustJSON(t, orderInput(p, 1, sess.Account.Email))))
	adminCSRFReq.Header.Set("Content-Type", "application/json")
	adminCSRFReq.Header.Set("Origin", "http://localhost:3000")
	adminCSRFReq.Header.Set("X-CSRF-Token", e.csrf.Value)
	adminCSRFReq.Header.Set("Idempotency-Key", phase06ID("wrong-csrf"))
	adminCSRFReq.AddCookie(sess.Session)
	adminCSRFReq.AddCookie(e.csrf)
	if rec := doReq(e.handler, adminCSRFReq); rec.Code != http.StatusForbidden {
		t.Fatalf("admin CSRF cookie satisfied customer route: %d", rec.Code)
	}

	adminAsCustomer := httptest.NewRequest(http.MethodGet, "/api/v1/customer/auth/me", nil)
	adminAsCustomer.Header.Set("Origin", "http://localhost:3000")
	adminAsCustomer.AddCookie(e.session)
	if rec := doReq(e.handler, adminAsCustomer); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin session satisfied customer route: %d", rec.Code)
	}
	customerAsAdmin := httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/me", nil)
	customerAsAdmin.Header.Set("Origin", "http://localhost:3000")
	customerAsAdmin.AddCookie(sess.Session)
	customerAsAdmin.AddCookie(sess.CSRF)
	if rec := doReq(e.handler, customerAsAdmin); rec.Code != http.StatusUnauthorized {
		t.Fatalf("customer session satisfied admin route: %d", rec.Code)
	}
	customerOrderAsAdmin := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(mustJSON(t, orderInput(p, 1, sess.Account.Email))))
	customerOrderAsAdmin.Header.Set("Content-Type", "application/json")
	customerOrderAsAdmin.Header.Set("Origin", "http://localhost:3000")
	customerOrderAsAdmin.Header.Set("X-CSRF-Token", sess.CSRFToken)
	customerOrderAsAdmin.Header.Set("Idempotency-Key", phase06ID("admin-principal"))
	customerOrderAsAdmin.AddCookie(e.session)
	customerOrderAsAdmin.AddCookie(sess.CSRF)
	if rec := doReq(e.handler, customerOrderAsAdmin); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin principal accepted by RequireCustomer: %d %s", rec.Code, rec.Body.String())
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/api/v1/orders", nil)
	preflight.Header.Set("Origin", "http://localhost:3000")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type,idempotency-key,x-csrf-token")
	rec = doReq(e.handler, preflight)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight: %d", rec.Code)
	}
	allowHeaders := rec.Header().Get("Access-Control-Allow-Headers")
	for _, header := range []string{"Idempotency-Key", "X-CSRF-Token"} {
		if !strings.Contains(allowHeaders, header) {
			t.Fatalf("preflight missing %s: %s", header, allowHeaders)
		}
	}
	badPreflight := httptest.NewRequest(http.MethodOptions, "/api/v1/orders", nil)
	badPreflight.Header.Set("Origin", "https://evil.example")
	badPreflight.Header.Set("Access-Control-Request-Method", "POST")
	if rec := doReq(e.handler, badPreflight); rec.Code != http.StatusForbidden {
		t.Fatalf("disallowed preflight accepted: %d", rec.Code)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
