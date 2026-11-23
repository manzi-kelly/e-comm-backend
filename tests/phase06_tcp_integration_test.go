//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"autivashop/backend/internal/customer"
	"autivashop/backend/internal/orders"
	"autivashop/backend/internal/server"
)

func TestPhase06ProductionTCPWorkflow(t *testing.T) {
	e := setupCustomerOrders(t)
	p := e.createPublishedProduct(t, 75000, 2)
	srv := server.New(e.cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	srv.SetProductService(e.svc)
	srv.SetCustomerService(e.customerSvc)
	srv.SetOrderService(e.orderSvc)
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
			t.Error("Phase 06 production server did not stop")
		}
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + listener.Addr().String() + "/api/v1"
	call := func(method, path, idem, csrfToken string, payload any, want int) []byte {
		t.Helper()
		var body io.Reader
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, base+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("Content-Type", "application/json")
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		if csrfToken != "" {
			req.Header.Set("X-CSRF-Token", csrfToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d body=%s", method, path, resp.StatusCode, want, b)
		}
		t.Logf("TCP %s %s -> %d", method, path, resp.StatusCode)
		return b
	}

	call(http.MethodGet, "/health", "", "", nil, http.StatusOK)
	call(http.MethodGet, "/ready", "", "", nil, http.StatusOK)
	bootstrap := call(http.MethodGet, "/customer/auth/me", "", "", nil, http.StatusUnauthorized)
	var tokenBody struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(bootstrap, &tokenBody); err != nil || tokenBody.CSRFToken == "" {
		t.Fatalf("missing TCP CSRF bootstrap: %s", bootstrap)
	}
	csrf := tokenBody.CSRFToken
	email := "phase06-tcp-" + phase06ID("cust") + "@example.test"
	password := "Customer-TCP-Password-123!"
	registerBody := call(http.MethodPost, "/customer/auth/register", "", csrf, map[string]string{
		"fullName": "Aline Umuhire", "email": email, "phone": uniqueRwandaPhone(),
		"password": password, "confirmPassword": password,
	}, http.StatusCreated)
	var authBody struct {
		Customer  customer.Profile `json:"customer"`
		CSRFToken string           `json:"csrfToken"`
	}
	if err := json.Unmarshal(registerBody, &authBody); err != nil || authBody.Customer.ID == "" {
		t.Fatalf("TCP registration failed: %s", registerBody)
	}
	e.cleanupCustomer(t, authBody.Customer.ID)
	csrf = authBody.CSRFToken
	me := call(http.MethodGet, "/customer/auth/me", "", "", nil, http.StatusOK)
	if !bytes.Contains(me, []byte(authBody.Customer.ID)) {
		t.Fatalf("TCP session not recognized: %s", me)
	}

	key := phase06ID("tcp-idem")
	created := call(http.MethodPost, "/orders", key, csrf, orderInput(p, 1, email), http.StatusCreated)
	var order orders.Order
	if err := json.Unmarshal(created, &order); err != nil || order.ID == "" || order.TotalRWF != 75000 {
		t.Fatalf("TCP order creation failed: %s", created)
	}
	call(http.MethodGet, "/orders/"+order.ID, "", "", nil, http.StatusOK)
	call(http.MethodGet, "/orders?limit=5", "", "", nil, http.StatusOK)
	call(http.MethodPost, "/customer/auth/logout", "", csrf, nil, http.StatusNoContent)
	call(http.MethodGet, "/orders", "", "", nil, http.StatusUnauthorized)
}
