//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/middleware"
	"autivashop/backend/internal/server"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
)

// Exercise the production router and Run lifecycle over loopback TCP with real
// PostgreSQL. The standalone cmd/api executable is verified separately.
func TestPhase02BLiveHTTP(t *testing.T) {
	e := setupAdminAuthEnv(t)
	password, err := auth.GenerateToken(32)
	if err != nil {
		t.Fatal(err)
	}
	u := e.createTestAdmin(t, "tcp+"+time.Now().Format("150405.000000000")+"@example.test", password, "TCP test", admin.RoleSuperAdmin, true)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	cfg := testConfig(t, map[string]string{"DATABASE_URL": os.Getenv("TEST_DATABASE_URL"), "API_HOST": "127.0.0.1", "API_PORT": strconv.Itoa(port), "CORS_ORIGINS": "http://localhost:3000", "CORS_ALLOW_CREDENTIALS": "true", "AUTH_COOKIE_SECURE": "false"})
	srv := server.New(cfg, e.db, testLogger())
	srv.SetAdminService(e.cfgSvc)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TCP server did not stop")
		}
	})
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1"
	for i := 0; i < 100; i++ {
		r, err := client.Get(base + "/health")
		if err == nil {
			_ = r.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	request := func(method, path string, payload any, csrf string, want int) map[string]any {
		t.Helper()
		var body bytes.Buffer
		if payload != nil {
			if err := json.NewEncoder(&body).Encode(payload); err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, base+path, &body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("Content-Type", "application/json")
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var result map[string]any
		_ = json.NewDecoder(r.Body).Decode(&result)
		if r.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d", method, path, r.StatusCode, want)
		}
		t.Logf("%s %s -> %d", method, path, r.StatusCode)
		return result
	}
	request("GET", "/health", nil, "", 200)
	request("GET", "/ready", nil, "", 200)
	bootstrap := request("GET", "/admin/auth/me", nil, "", 401)
	csrf, _ := bootstrap["csrfToken"].(string)
	if csrf == "" {
		t.Fatal("missing bootstrap token")
	}
	creds := map[string]string{"email": u.Email, "password": password}
	request("POST", "/admin/auth/login", creds, "wrong", 403)
	login := request("POST", "/admin/auth/login", creds, csrf, 200)
	csrf = login["csrfToken"].(string)
	request("GET", "/admin/auth/me", nil, "", 200)
	url, _ := http.NewRequest("GET", base+"/admin/auth/me", nil)
	saved := jar.Cookies(url.URL)
	for _, c := range saved {
		c.Path = "/"
	}
	request("POST", "/admin/auth/logout", nil, csrf, 204)
	jar.SetCookies(url.URL, saved)
	expired := request("GET", "/admin/auth/me", nil, "", 401)
	csrf = expired["csrfToken"].(string)
	// A revoked session response must leave a usable CSRF cookie for re-login.
	login = request("POST", "/admin/auth/login", creds, csrf, 200)
	csrf = login["csrfToken"].(string)
	saved = jar.Cookies(url.URL)
	for _, c := range saved {
		c.Path = "/"
	}
	request("POST", "/admin/auth/logout-all", nil, csrf, 200)
	jar.SetCookies(url.URL, saved)
	request("GET", "/admin/auth/me", nil, "", 401)
	jar.SetCookies(url.URL, []*http.Cookie{{Name: e.cookies.SessionName, Value: "invalid-session", Path: "/"}})
	request("GET", "/admin/auth/me", nil, "", 401)
	request("POST", "/admin/auth/logout", nil, "", 403)
}

func TestPhase02BExpirationRateLimitAndRollback(t *testing.T) {
	e := setupAdminAuthEnv(t)
	u := e.createTestAdmin(t, "phase02b+"+time.Now().Format("150405.000000000")+"@example.test", "Phase02B-Temporary-Password!", "Isolated", admin.RoleAdmin, true)
	user, sess, token, _, err := e.cfgSvc.Login(e.ctx, u.Email, "Phase02B-Temporary-Password!", admin.LoginMeta{IP: "127.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	// Fail audit insertion only for this isolated user's logout. Revocation must roll back.
	_, err = e.db.Exec(e.ctx, `ALTER TABLE admin_audit_events ADD CONSTRAINT phase02b_reject_logout CHECK (NOT (action = 'admin.logout' AND admin_user_id = '`+u.ID+`'::uuid)) NOT VALID`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.db.Exec(context.Background(), `ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS phase02b_reject_logout`)
	})
	if err := e.cfgSvc.Logout(e.ctx, user, sess); err == nil {
		t.Fatal("expected audit failure")
	}
	if _, _, err := e.cfgSvc.Authenticate(e.ctx, token); err != nil {
		t.Fatal("logout revoked session despite audit rollback")
	}
	if _, err = e.db.Exec(e.ctx, `ALTER TABLE admin_audit_events DROP CONSTRAINT phase02b_reject_logout`); err != nil {
		t.Fatal(err)
	}
	if _, err = e.db.Exec(e.ctx, `UPDATE admin_sessions SET created_at=now()-interval '2 hours', expires_at=now()-interval '1 hour' WHERE id=$1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: e.cookies.SessionName, Value: token})
	if got := doReq(e.handler, req).Code; got != 401 {
		t.Fatalf("expired session: %d", got)
	}
	csrf, cookies := getCSRFToken(t, e.handler)
	for i := 0; i < 6; i++ {
		rec := loginReq(t, e.handler, u.Email, "incorrect-password", csrf, cookies)
		want := 401
		if i == 5 {
			want = 429
		}
		if rec.Code != want {
			t.Fatalf("attempt %d: %d, want %d", i, rec.Code, want)
		}
	}
}

func TestPhase02BConstraints(t *testing.T) {
	e := setupAdminAuthEnv(t)
	u := e.createTestAdmin(t, "constraints+"+time.Now().Format("150405.000000000")+"@example.test", "Phase02B-Temporary-Password!", "Constraint", admin.RoleAdmin, true)
	cases := []struct {
		sql, code string
		args      []any
	}{
		{`INSERT INTO admin_users(email,password_hash,display_name,role) SELECT upper(email),password_hash,display_name,role FROM admin_users WHERE id=$1`, "23505", []any{u.ID}},
		{`UPDATE admin_users SET role='customer' WHERE id=$1`, "23514", []any{u.ID}},
		{`UPDATE admin_users SET email='invalid' WHERE id=$1`, "23514", []any{u.ID}},
		{`INSERT INTO admin_sessions(admin_user_id,token_hash,expires_at) VALUES ($1,decode('01','hex'),now()-interval '1 hour')`, "23514", []any{u.ID}},
		{`INSERT INTO admin_sessions(admin_user_id,token_hash,expires_at) VALUES ('00000000-0000-0000-0000-000000000000',decode('02','hex'),now()+interval '1 hour')`, "23503", nil},
		{`INSERT INTO admin_audit_events(action) VALUES ('')`, "23514", nil},
	}
	for _, tc := range cases {
		tx, err := e.db.Pool().Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(e.ctx, tc.sql, tc.args...)
		_ = tx.Rollback(e.ctx)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != tc.code {
			t.Fatalf("constraint: got %v, want %s", err, tc.code)
		}
	}
}

func TestPhase02BRoleAuthorization(t *testing.T) {
	h := middleware.RequireAuthenticated(middleware.RequireActiveAccount(middleware.RequireRoles("super_admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))))
	for _, tc := range []struct {
		role            string
		active, present bool
		want            int
	}{{"", false, false, 401}, {"admin", true, true, 403}, {"super_admin", false, true, 403}, {"super_admin", true, true, 204}} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.present {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.AdminPrincipal{Role: tc.role, IsActive: tc.active}))
		}
		if got := doReq(h, r).Code; got != tc.want {
			t.Fatalf("role %s active %v: %d", tc.role, tc.active, got)
		}
	}
}

func TestPhase02BBootstrapSerialization(t *testing.T) {
	e := setupAdminAuthEnv(t)
	tx, err := e.db.Pool().Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	repo := e.cfgSvc.Repository()
	n, err := repo.CountUsersTx(e.ctx, tx)
	if err != nil || n != 0 {
		t.Fatalf("isolated bootstrap count: %d %v", n, err)
	}
	password, err := auth.GenerateToken(32)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	u, err := repo.CreateUserTx(e.ctx, tx, "bootstrap-serialization@example.test", hash, "Bootstrap", admin.RoleSuperAdmin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(context.Background(), `DELETE FROM admin_users WHERE id=$1`, u.ID) })
	uid := u.ID
	if err := repo.InsertAuditEventTx(e.ctx, tx, &uid, "admin.bootstrap.created", map[string]any{"role": string(u.Role)}); err != nil {
		t.Fatal(err)
	}
	// A competing bootstrap must wait for the initial transaction, then observe
	// the committed administrator rather than another empty database.
	second, err := e.db.Pool().Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(context.Background())
	waiting, cancel := context.WithTimeout(e.ctx, 100*time.Millisecond)
	_, err = repo.CountUsersTx(waiting, second)
	cancel()
	if err == nil {
		t.Fatal("concurrent bootstrap count did not wait")
	}
	_ = second.Rollback(context.Background())
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	second, err = e.db.Pool().Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(context.Background())
	n, err = repo.CountUsersTx(e.ctx, second)
	if err != nil || n != 1 {
		t.Fatalf("second bootstrap must see existing admin: %d %v", n, err)
	}
	var audits int
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM admin_audit_events WHERE admin_user_id=$1 AND action='admin.bootstrap.created'`, u.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("bootstrap audit: %d %v", audits, err)
	}
}
