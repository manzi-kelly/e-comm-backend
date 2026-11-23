//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/database"
	"autivashop/backend/internal/server"
)

type adminAuthEnv struct {
	db      *database.DB
	handler http.Handler
	cfgSvc  *admin.Service
	cookies auth.CookieConfig
	ctx     context.Context
	email   string
	pass    string
}

func setupAdminAuthEnv(t *testing.T) *adminAuthEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; admin auth integration tests skipped")
	}
	cfg := testConfig(t, map[string]string{
		"DATABASE_URL":              dsn,
		"DB_QUERY_TIMEOUT":          "5s",
		"AUTH_SESSION_TTL":          "30m",
		"AUTH_COOKIE_SECURE":        "false",
		"AUTH_COOKIE_SAMESITE":      "lax",
		"CORS_ORIGINS":              "http://localhost:3000,http://127.0.0.1:3000",
		"CORS_ALLOW_CREDENTIALS":    "true",
		"AUTH_LOGIN_MAX_FAILURES":   "5",
		"AUTH_LOGIN_LOCKOUT_WINDOW": "2m",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		t.Skipf("dedicated PostgreSQL test database is unreachable: %v", err)
	}
	t.Cleanup(db.Close)

	if _, err := db.Migrate(ctx, filepath.Join("..", "migrations")); err != nil {
		t.Fatalf("Migrate() failed: %v", err)
	}

	// Isolate test data in a transaction-like manner is not possible across HTTP handlers;
	// use unique emails per test and best-effort cleanup of created rows.
	svc := admin.NewService(db.Pool(), cfg, testLogger())
	srv := server.New(cfg, db, testLogger())
	srv.SetAdminService(svc)
	return &adminAuthEnv{
		db:      db,
		handler: srv.Handler(),
		cfgSvc:  svc,
		cookies: auth.NewCookieConfig(cfg.Auth),
		ctx:     ctx,
	}
}

func (e *adminAuthEnv) createTestAdmin(t *testing.T, email, password, displayName string, role admin.Role, active bool) admin.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user, err := e.cfgSvc.Repository().CreateUser(e.ctx, email, hash, displayName, role)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if !active {
		if _, err := e.db.Exec(e.ctx, `UPDATE admin_users SET is_active = false, updated_at = now() WHERE id = $1`, user.ID); err != nil {
			t.Fatalf("deactivate user: %v", err)
		}
		user.IsActive = false
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.db.Exec(ctx, `DELETE FROM admin_users WHERE id = $1`, user.ID)
	})
	return user
}

func doReq(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func getCSRFToken(t *testing.T, handler http.Handler) (string, []*http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/me", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := doReq(handler, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 from /me bootstrap, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	token, _ := body["csrfToken"].(string)
	if token == "" {
		// fallback: read from Set-Cookie
		for _, c := range rec.Result().Cookies() {
			if strings.Contains(c.Name, "csrf") {
				token = c.Value
			}
		}
	}
	if token == "" {
		t.Fatalf("missing csrf token in /me response: %s", rec.Body.String())
	}
	return token, rec.Result().Cookies()
}

func loginReq(t *testing.T, handler http.Handler, email, password, csrf string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", csrf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return doReq(handler, req)
}

func TestAdminAuthIntegration_LoginLogoutMeAndSessions(t *testing.T) {
	env := setupAdminAuthEnv(t)
	email := "admin+" + time.Now().Format("150405000") + "@example.test"
	password := "Valid-Admin-Password-123!"
	user := env.createTestAdmin(t, email, password, "Test Admin", admin.RoleAdmin, true)

	csrf, cookies := getCSRFToken(t, env.handler)
	rec := loginReq(t, env.handler, email, password, csrf, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var loginBody struct {
		Admin struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"admin"`
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loginBody); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if loginBody.Admin.ID != user.ID || loginBody.Admin.Email == "" || loginBody.Admin.Role != string(admin.RoleAdmin) || loginBody.CSRFToken == "" {
		t.Fatalf("unexpected login response: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "password") || strings.Contains(rec.Body.String(), "token_hash") {
		t.Fatal("login response leaked sensitive fields")
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == env.cookies.SessionName {
			sessionCookie = c
		}
		if c.Name == env.cookies.CSRFName {
			csrfCookie = c
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("missing auth cookies: %#v", rec.Result().Cookies())
	}
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie attributes incorrect: %#v", sessionCookie)
	}
	if csrfCookie.HttpOnly {
		t.Fatal("csrf cookie must be readable for double-submit pattern")
	}

	// /me with session
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/me", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	rec = doReq(env.handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/me status=%d body=%s", rec.Code, rec.Body.String())
	}

	// logout
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/logout", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", csrfCookie.Value)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	rec = doReq(env.handler, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", rec.Code, rec.Body.String())
	}

	// revoked session rejected
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/me", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.AddCookie(sessionCookie)
	rec = doReq(env.handler, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", rec.Code)
	}

	// session persistence: row exists and revoked
	var revoked bool
	err := env.db.Pool().QueryRow(env.ctx, `SELECT revoked_at IS NOT NULL FROM admin_sessions WHERE admin_user_id = $1 ORDER BY created_at DESC LIMIT 1`, user.ID).Scan(&revoked)
	if err != nil || !revoked {
		t.Fatalf("expected revoked session in db, err=%v revoked=%v", err, revoked)
	}
	var auditCount int
	err = env.db.Pool().QueryRow(env.ctx, `SELECT COUNT(*) FROM admin_audit_events WHERE admin_user_id = $1 AND action IN ('admin.login.success','admin.logout')`, user.ID).Scan(&auditCount)
	if err != nil || auditCount < 2 {
		t.Fatalf("expected login/logout audit events, err=%v count=%d", err, auditCount)
	}
}

func TestAdminAuthIntegration_InvalidCredentialsDisabledAndUnknown(t *testing.T) {
	env := setupAdminAuthEnv(t)
	pass := "Valid-Admin-Password-123!"
	activeEmail := "active+" + time.Now().Format("150405001") + "@example.test"
	disabledEmail := "disabled+" + time.Now().Format("150405001") + "@example.test"
	env.createTestAdmin(t, activeEmail, pass, "Active", admin.RoleAdmin, true)
	env.createTestAdmin(t, disabledEmail, pass, "Disabled", admin.RoleAdmin, false)

	for _, tc := range []struct{ email, password string }{
		{activeEmail, "wrong-password-0000"},
		{"unknown+" + time.Now().Format("150405001") + "@example.test", "wrong-password-0000"},
		{disabledEmail, pass},
	} {
		csrf, cookies := getCSRFToken(t, env.handler)
		rec := loginReq(t, env.handler, tc.email, tc.password, csrf, cookies)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for %#v, got %d body=%s", tc, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Invalid email or password") {
			t.Fatalf("expected generic auth error, got %s", rec.Body.String())
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "disabled") || strings.Contains(strings.ToLower(rec.Body.String()), "not found") {
			t.Fatalf("response leaked account state: %s", rec.Body.String())
		}
	}
}

func TestAdminAuthIntegration_CSRFOriginLogoutAllAndSessionFixation(t *testing.T) {
	env := setupAdminAuthEnv(t)
	email := "fixation+" + time.Now().Format("150405002") + "@example.test"
	pass := "Valid-Admin-Password-123!"
	env.createTestAdmin(t, email, pass, "Admin", admin.RoleSuperAdmin, true)

	csrf, cookies := getCSRFToken(t, env.handler)
	// login with bad origin rejected
	payload, _ := json.Marshal(map[string]string{"email": email, "password": pass})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("X-CSRF-Token", csrf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := doReq(env.handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for bad origin login, got %d body=%s", rec.Code, rec.Body.String())
	}

	// login without CSRF rejected
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec = doReq(env.handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing csrf login, got %d body=%s", rec.Code, rec.Body.String())
	}

	// successful login 1
	rec = loginReq(t, env.handler, email, pass, csrf, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("login1 failed: %d %s", rec.Code, rec.Body.String())
	}
	var sess1 string
	for _, c := range rec.Result().Cookies() {
		if c.Name == env.cookies.SessionName {
			sess1 = c.Value
		}
	}
	// successful login 2 (session fixation resistance / multiple sessions)
	csrf2pre, cookies2pre := getCSRFToken(t, env.handler)
	rec = loginReq(t, env.handler, email, pass, csrf2pre, cookies2pre)
	if rec.Code != http.StatusOK {
		t.Fatalf("login2 failed: %d %s", rec.Code, rec.Body.String())
	}
	var sess2, csrf2 string
	for _, c := range rec.Result().Cookies() {
		if c.Name == env.cookies.SessionName {
			sess2 = c.Value
		}
		if c.Name == env.cookies.CSRFName {
			csrf2 = c.Value
		}
	}
	if sess1 == "" || sess2 == "" || sess1 == sess2 {
		t.Fatal("each login must rotate/create a unique session identity")
	}

	// logout-all with session 2
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/logout-all", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", csrf2)
	req.AddCookie(&http.Cookie{Name: env.cookies.SessionName, Value: sess2})
	req.AddCookie(&http.Cookie{Name: env.cookies.CSRFName, Value: csrf2})
	rec = doReq(env.handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout-all status=%d body=%s", rec.Code, rec.Body.String())
	}

	// both sessions invalid
	for _, s := range []string{sess1, sess2} {
		req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/me", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		req.AddCookie(&http.Cookie{Name: env.cookies.SessionName, Value: s})
		rec = doReq(env.handler, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected session revoked after logout-all, got %d", rec.Code)
		}
	}
}

func TestAdminAuthIntegration_DisabledAccountInvalidatesExistingSession(t *testing.T) {
	env := setupAdminAuthEnv(t)
	email := "disable+" + time.Now().Format("150405003") + "@example.test"
	pass := "Valid-Admin-Password-123!"
	user := env.createTestAdmin(t, email, pass, "Admin", admin.RoleAdmin, true)

	csrf, cookies := getCSRFToken(t, env.handler)
	rec := loginReq(t, env.handler, email, pass, csrf, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var sessCookie, csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == env.cookies.SessionName {
			sessCookie = c
		}
		if c.Name == env.cookies.CSRFName {
			csrfCookie = c
		}
	}
	if _, err := env.db.Exec(env.ctx, `UPDATE admin_users SET is_active = false, updated_at = now() WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/me", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.AddCookie(sessCookie)
	req.AddCookie(csrfCookie)
	rec = doReq(env.handler, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled account session should be rejected, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminAuthIntegration_MalformedInputAndUnauthorized(t *testing.T) {
	env := setupAdminAuthEnv(t)
	csrf, cookies := getCSRFToken(t, env.handler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/login", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", csrf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := doReq(env.handler, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed login should be 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/session-check", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec = doReq(env.handler, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated session-check should be 401, got %d", rec.Code)
	}
}
