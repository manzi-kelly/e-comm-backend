//go:build !js

package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/adminactivation"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/database"
	"autivashop/backend/internal/server"
)

const activationPassword = "Autiva!Secure#2026Activation"

type fakeActivationSender struct {
	mu     sync.Mutex
	codes  []string
	to     []string
	fail   bool
	failed int
}

func (f *fakeActivationSender) SendActivationCode(_ context.Context, to, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		f.failed++
		return errors.New("controlled fake delivery failure")
	}
	f.codes = append(f.codes, code)
	f.to = append(f.to, to)
	return nil
}

func (f *fakeActivationSender) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeActivationSender) snapshot() ([]string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.codes...), append([]string(nil), f.to...), f.failed
}

type activationClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *activationClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *activationClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type activationEnv struct {
	db      *database.DB
	svc     *adminactivation.Service
	admin   *admin.Service
	handler http.Handler
	sender  *fakeActivationSender
	clock   *activationClock
	cfg     config.Config
	ctx     context.Context
}

func activationTestConfig(t *testing.T, dsn string) config.Config {
	t.Helper()
	return testConfig(t, map[string]string{
		"DATABASE_URL":                         dsn,
		"CORS_ORIGINS":                         "http://localhost:3000,http://127.0.0.1:3000",
		"CORS_ALLOW_CREDENTIALS":               "true",
		"AUTH_COOKIE_SECURE":                   "false",
		"ADMIN_ACTIVATION_OTP_KEY":             base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)),
		"ADMIN_ACTIVATION_SMTP_HOST":           "localhost",
		"ADMIN_ACTIVATION_SMTP_FROM":           "no-reply@autivashop.test",
		"ADMIN_ACTIVATION_SMTP_TLS":            "none",
		"ADMIN_ACTIVATION_MAX_VERIFY_ATTEMPTS": "3",
		"ADMIN_ACTIVATION_MAX_RESENDS":         "3",
		"ADMIN_ACTIVATION_RESEND_WINDOW":       "1h",
		"ADMIN_ACTIVATION_REQUEST_COOLDOWN":    "60s",
		"ADMIN_ACTIVATION_AUTH_TTL":            "5m",
	})
}

func resetActivationState(t *testing.T, db *database.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	statements := []string{
		`DELETE FROM admin_activation_codes`,
		`DELETE FROM admin_activation_authorizations`,
		`DELETE FROM admin_users WHERE email = lower($1)`,
		`DELETE FROM admin_audit_events`,
	}
	for _, stmt := range statements {
		var err error
		if strings.Contains(stmt, "$1") {
			_, err = db.Exec(ctx, stmt, config.AuthorizedAdminEmail)
		} else {
			_, err = db.Exec(ctx, stmt)
		}
		if err != nil {
			t.Fatalf("activation test cleanup failed: %v", err)
		}
	}
}

func newActivationEnv(t *testing.T) *activationEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; admin activation integration tests skipped")
	}
	cfg := activationTestConfig(t, dsn)
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
	resetActivationState(t, db)

	sender := &fakeActivationSender{}
	svc, err := adminactivation.NewService(db.Pool(), cfg.AdminActivation, sender, testLogger())
	if err != nil {
		t.Fatalf("NewService() failed: %v", err)
	}
	clock := &activationClock{t: time.Now().UTC()}
	svc.SetNow(clock.now)
	adminSvc := admin.NewService(db.Pool(), cfg, testLogger())
	srv := server.New(cfg, db, testLogger())
	srv.SetAdminService(adminSvc)
	srv.SetAdminActivationService(svc)

	env := &activationEnv{
		db: db, svc: svc, admin: adminSvc, handler: srv.Handler(),
		sender: sender, clock: clock, cfg: cfg, ctx: ctx,
	}
	t.Cleanup(func() { resetActivationState(t, db) })
	return env
}

func activationPost(t *testing.T, handler http.Handler, path string, payload any, csrf string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func activationCSRF(t *testing.T, handler http.Handler) (string, []*http.Cookie) {
	t.Helper()
	return getCSRFToken(t, handler)
}

func decodeActivationError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error response is not JSON: %s", rec.Body.String())
	}
	return envelope.Error.Code + ":" + envelope.Error.Message
}

func TestActivationCodeGenerationAndKeyedHash(t *testing.T) {
	first, err := adminactivation.GenerateCode()
	if err != nil || !adminactivation.ValidCodeFormat(first) {
		t.Fatalf("GenerateCode() = %q, %v", first, err)
	}
	second, err := adminactivation.GenerateCode()
	if err != nil || !adminactivation.ValidCodeFormat(second) {
		t.Fatalf("GenerateCode() = %q, %v", second, err)
	}
	if first == second {
		t.Fatal("two generated codes were identical")
	}
	key := bytes.Repeat([]byte{0x11}, 32)
	hash := adminactivation.HashCode(key, config.AuthorizedAdminEmail, first)
	if len(hash) != 32 {
		t.Fatalf("hash length = %d, want 32", len(hash))
	}
	if !adminactivation.CodeMatches(key, config.AuthorizedAdminEmail, first, hash) {
		t.Fatal("keyed hash did not verify")
	}
	if adminactivation.CodeMatches(key, "other@example.com", first, hash) {
		t.Fatal("code hash was not bound to the authorized email")
	}
	if adminactivation.CodeMatches(bytes.Repeat([]byte{0x22}, 32), config.AuthorizedAdminEmail, first, hash) {
		t.Fatal("code hash verified under a different key")
	}
}

func TestActivationConfigurationFailsClosed(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"ADMIN_ACTIVATION_OTP_KEY":   "",
		"ADMIN_ACTIVATION_SMTP_HOST": "",
	})
	if cfg.AdminActivation.Enabled {
		t.Fatal("activation reported enabled without OTP key or SMTP host")
	}
	joined := strings.Join(cfg.AdminActivation.SetupIssues, ",")
	if !strings.Contains(joined, "ADMIN_ACTIVATION_OTP_KEY") || !strings.Contains(joined, "ADMIN_ACTIVATION_SMTP_HOST") {
		t.Fatalf("setup issues did not name missing settings: %s", joined)
	}
	if _, err := adminactivation.NewService(nil, cfg.AdminActivation, &fakeActivationSender{}, testLogger()); !errors.Is(err, adminactivation.ErrUnconfigured) {
		t.Fatalf("NewService() error = %v, want ErrUnconfigured", err)
	}
}

func TestActivationProvisioningGuardsAndLoginRejection(t *testing.T) {
	env := newActivationEnv(t)

	created, err := env.svc.Provision(env.ctx, "AutivaShop Administrator")
	if err != nil || !created {
		t.Fatalf("first Provision() = %v, %v", created, err)
	}
	created, err = env.svc.Provision(env.ctx, "AutivaShop Administrator")
	if err != nil || created {
		t.Fatalf("second Provision() = %v, %v", created, err)
	}

	var adminCount int
	if err := env.db.Pool().QueryRow(env.ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&adminCount); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if adminCount != 0 {
		t.Fatalf("provisioning created %d administrator accounts", adminCount)
	}

	csrf, cookies := activationCSRF(t, env.handler)
	rec := activationPost(t, env.handler, "/api/v1/admin/auth/login", map[string]string{
		"email": config.AuthorizedAdminEmail, "password": activationPassword,
	}, csrf, cookies)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login before activation status = %d, want 401", rec.Code)
	}

	otherHash, err := auth.HashPassword(activationPassword)
	if err != nil {
		t.Fatalf("hash existing admin password: %v", err)
	}
	existing, err := env.admin.Repository().CreateUser(env.ctx, "existing.admin@example.com", otherHash,
		"Existing Admin", admin.RoleAdmin)
	if err != nil {
		t.Fatalf("create existing admin: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `DELETE FROM admin_users WHERE id = $1`, existing.ID)
	})
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); !errors.Is(err, adminactivation.ErrAdminExists) {
		t.Fatalf("Provision() with existing admin error = %v, want ErrAdminExists", err)
	}
	after, err := env.admin.Repository().FindUserByID(env.ctx, existing.ID)
	if err != nil || after.PasswordHash != otherHash || after.Role != admin.RoleAdmin {
		t.Fatalf("existing administrator was modified: %+v, %v", after, err)
	}
}

func TestActivationAuthorizedEmailFullWorkflow(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	csrf, cookies := activationCSRF(t, env.handler)

	rec := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request-code status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "code") {
		t.Fatalf("request-code response leaked code information: %s", rec.Body.String())
	}
	codes, recipients, _ := env.sender.snapshot()
	if len(codes) != 1 || codes[0] == "" || recipients[0] != config.AuthorizedAdminEmail {
		t.Fatalf("fake sender captured %d codes / %d recipients", len(codes), len(recipients))
	}
	code := codes[0]

	wrong := "000000"
	if code == wrong {
		wrong = "999999"
	}
	rec = activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
		map[string]string{"email": config.AuthorizedAdminEmail, "code": wrong}, csrf, cookies)
	if rec.Code == http.StatusOK {
		t.Fatalf("wrong code was accepted: %s", rec.Body.String())
	}

	rec = activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
		map[string]string{"email": config.AuthorizedAdminEmail, "code": code}, csrf, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify-code status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var verified struct {
		Status          string `json:"status"`
		ActivationToken string `json:"activationToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verified); err != nil || verified.ActivationToken == "" {
		t.Fatalf("verify response missing activation token: %s", rec.Body.String())
	}

	rec = activationPost(t, env.handler, "/api/v1/admin/activation/complete", map[string]string{
		"email": config.AuthorizedAdminEmail, "activationToken": verified.ActivationToken, "password": "short",
	}, csrf, cookies)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("weak password status = %d, want 422: %s", rec.Code, rec.Body.String())
	}

	rec = activationPost(t, env.handler, "/api/v1/admin/activation/complete", map[string]string{
		"email": config.AuthorizedAdminEmail, "activationToken": verified.ActivationToken, "password": activationPassword,
	}, csrf, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var completed struct {
		Status string `json:"status"`
		Admin  struct {
			Email    string `json:"email"`
			Role     string `json:"role"`
			IsActive bool   `json:"isActive"`
		} `json:"admin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &completed); err != nil ||
		completed.Admin.Role != string(admin.RoleSuperAdmin) || !completed.Admin.IsActive {
		t.Fatalf("completion response did not expose active super_admin: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), config.AuthorizedAdminEmail) {
		t.Fatalf("completion response missing expected admin profile: %s", rec.Body.String())
	}

	user, err := env.admin.Repository().FindUserByEmail(env.ctx, config.AuthorizedAdminEmail)
	if err != nil {
		t.Fatalf("activated admin not found: %v", err)
	}
	if !strings.HasPrefix(user.PasswordHash, "$argon2id$") || auth.VerifyPassword(activationPassword, user.PasswordHash) != nil {
		t.Fatal("activated administrator password was not stored as a valid Argon2id hash")
	}
	if strings.Contains(user.PasswordHash, activationPassword) {
		t.Fatal("plaintext password was stored")
	}

	// Replay of the single-use authorization must fail.
	rec = activationPost(t, env.handler, "/api/v1/admin/activation/complete", map[string]string{
		"email": config.AuthorizedAdminEmail, "activationToken": verified.ActivationToken, "password": activationPassword,
	}, csrf, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("token replay status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Existing admin login remains functional after activation.
	rec = activationPost(t, env.handler, "/api/v1/admin/auth/login", map[string]string{
		"email": config.AuthorizedAdminEmail, "password": activationPassword,
	}, csrf, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after activation status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// No additional mail is sent for an already activated address.
	before, _, _ := env.sender.snapshot()
	rec = activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies)
	after, _, _ := env.sender.snapshot()
	if rec.Code != http.StatusAccepted || len(after) != len(before) {
		t.Fatalf("request-code after activation status = %d, mail count %d -> %d", rec.Code, len(before), len(after))
	}
}

func TestActivationUnauthorizedEmailHasNoEnumeration(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	csrf, cookies := activationCSRF(t, env.handler)

	unauthorized := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": "attacker@example.com"}, csrf, cookies)
	authorized := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies)
	if unauthorized.Code != authorized.Code || unauthorized.Body.String() != authorized.Body.String() {
		t.Fatalf("request responses differ: unauthorized=%s authorized=%s", unauthorized.Body.String(), authorized.Body.String())
	}
	codes, recipients, _ := env.sender.snapshot()
	if len(codes) != 1 || recipients[0] != config.AuthorizedAdminEmail {
		t.Fatalf("mail was sent to an unauthorized address: %v", recipients)
	}

	verifyUnauthorized := activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
		map[string]string{"email": "attacker@example.com", "code": "123456"}, csrf, cookies)
	verifyAuthorized := activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
		map[string]string{"email": config.AuthorizedAdminEmail, "code": "654321"}, csrf, cookies)
	if verifyUnauthorized.Code != verifyAuthorized.Code || decodeActivationError(t, verifyUnauthorized) != decodeActivationError(t, verifyAuthorized) {
		t.Fatal("verify responses revealed whether the email is authorized")
	}

	complete := activationPost(t, env.handler, "/api/v1/admin/activation/complete", map[string]string{
		"email": "attacker@example.com", "activationToken": "invalid-token", "password": activationPassword,
	}, csrf, cookies)
	if complete.Code != http.StatusBadRequest {
		t.Fatalf("unauthorized completion status = %d, want 400", complete.Code)
	}
	var adminCount int
	if err := env.db.Pool().QueryRow(env.ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&adminCount); err != nil || adminCount != 0 {
		t.Fatalf("unauthorized completion created an admin: count=%d err=%v", adminCount, err)
	}
}

func TestActivationExpiredCode(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	csrf, cookies := activationCSRF(t, env.handler)
	if rec := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies); rec.Code != http.StatusAccepted {
		t.Fatalf("request-code status = %d", rec.Code)
	}
	codes, _, _ := env.sender.snapshot()
	if len(codes) != 1 {
		t.Fatalf("expected one code, got %d", len(codes))
	}

	env.clock.advance(adminactivation.ActivationCodeTTL + time.Minute)
	rec := activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
		map[string]string{"email": config.AuthorizedAdminEmail, "code": codes[0]}, csrf, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expired code status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var live int
	if err := env.db.Pool().QueryRow(env.ctx, `
		SELECT COUNT(*) FROM admin_activation_codes WHERE consumed_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("expired code was not consumed: live=%d err=%v", live, err)
	}
}

func TestActivationAttemptAndResendThrottles(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	csrf, cookies := activationCSRF(t, env.handler)
	request := func(email string) *httptest.ResponseRecorder {
		return activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
			map[string]string{"email": email}, csrf, cookies)
	}
	if rec := request(config.AuthorizedAdminEmail); rec.Code != http.StatusAccepted {
		t.Fatalf("first request status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := request(config.AuthorizedAdminEmail); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("resend cooldown status = %d, want 429: %s", rec.Code, rec.Body.String())
	}

	codes, _, _ := env.sender.snapshot()
	wrong := "000000"
	if codes[0] == wrong {
		wrong = "111111"
	}
	for i := 0; i < 3; i++ {
		if rec := activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
			map[string]string{"email": config.AuthorizedAdminEmail, "code": wrong}, csrf, cookies); rec.Code != http.StatusBadRequest {
			t.Fatalf("wrong verify %d status = %d, want 400", i, rec.Code)
		}
	}
	var consumed int
	if err := env.db.Pool().QueryRow(env.ctx, `
		SELECT COUNT(*) FROM admin_activation_codes WHERE consumed_at IS NOT NULL`).Scan(&consumed); err != nil || consumed == 0 {
		t.Fatalf("exhausted code was not consumed: consumed=%d err=%v", consumed, err)
	}
	if rec := activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
		map[string]string{"email": config.AuthorizedAdminEmail, "code": codes[0]}, csrf, cookies); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct code after attempt exhaustion status = %d, want 429", rec.Code)
	}

	// Advance beyond the resend window to clear durable and in-memory counters.
	env.clock.advance(2 * time.Hour)
	if rec := request(config.AuthorizedAdminEmail); rec.Code != http.StatusAccepted {
		t.Fatalf("request after window status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := request(config.AuthorizedAdminEmail); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request after window status = %d, want 429", rec.Code)
	}
	if rec := request("attacker@example.com"); rec.Code != http.StatusAccepted {
		t.Fatalf("unauthorized IP request status = %d, want 202", rec.Code)
	}
	if rec := request("attacker@example.com"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("IP throttle status = %d, want 429", rec.Code)
	}
}

func TestActivationConcurrentVerificationAndCompletion(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	csrf, cookies := activationCSRF(t, env.handler)
	if rec := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies); rec.Code != http.StatusAccepted {
		t.Fatalf("request-code status = %d", rec.Code)
	}
	codes, _, _ := env.sender.snapshot()

	const workers = 8
	tokens := make(chan string, workers)
	var verifyWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		verifyWG.Add(1)
		go func() {
			defer verifyWG.Done()
			token, err := env.svc.VerifyCode(env.ctx, config.AuthorizedAdminEmail, codes[0], "127.0.0.1")
			if err == nil {
				tokens <- token
			}
		}()
	}
	verifyWG.Wait()
	close(tokens)
	winner := ""
	for token := range tokens {
		if winner != "" {
			t.Fatal("multiple concurrent verifications succeeded")
		}
		winner = token
	}
	if winner == "" {
		t.Fatal("no concurrent verification succeeded")
	}

	var completeWG sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		completeWG.Add(1)
		go func() {
			defer completeWG.Done()
			_, err := env.svc.Complete(env.ctx, config.AuthorizedAdminEmail, winner, activationPassword, "127.0.0.1")
			results <- err
		}()
	}
	completeWG.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent completions succeeded %d times, want 1", successes)
	}
	var adminCount int
	if err := env.db.Pool().QueryRow(env.ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&adminCount); err != nil || adminCount != 1 {
		t.Fatalf("admins after concurrent activation = %d, err = %v", adminCount, err)
	}
}

func TestActivationDatabaseRollbackAndEmailFailure(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	csrf, cookies := activationCSRF(t, env.handler)

	env.sender.setFail(true)
	rec := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("delivery failure status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	var live int
	if err := env.db.Pool().QueryRow(env.ctx, `SELECT COUNT(*) FROM admin_activation_codes WHERE consumed_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("failed delivery left a usable code: live=%d err=%v", live, err)
	}
	env.sender.setFail(false)
	env.clock.advance(2 * time.Hour)
	if rec := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies); rec.Code != http.StatusAccepted {
		t.Fatalf("retry request status = %d: %s", rec.Code, rec.Body.String())
	}

	codes, _, _ := env.sender.snapshot()
	verify := activationPost(t, env.handler, "/api/v1/admin/activation/verify-code",
		map[string]string{"email": config.AuthorizedAdminEmail, "code": codes[len(codes)-1]}, csrf, cookies)
	if verify.Code != http.StatusOK {
		t.Fatalf("verify status = %d: %s", verify.Code, verify.Body.String())
	}
	var verified struct {
		ActivationToken string `json:"activationToken"`
	}
	_ = json.Unmarshal(verify.Body.Bytes(), &verified)

	const functionName = "autiva_activation_fail_audit"
	ddl := []string{
		`CREATE OR REPLACE FUNCTION ` + functionName + `() RETURNS trigger AS $$ BEGIN
			IF NEW.action = 'admin.activation.completed' THEN RAISE EXCEPTION 'controlled rollback'; END IF;
			RETURN NEW; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER autiva_activation_fail_audit_trg BEFORE INSERT ON admin_audit_events
			FOR EACH ROW EXECUTE FUNCTION ` + functionName + `()`,
	}
	for _, stmt := range ddl {
		if _, err := env.db.Exec(env.ctx, stmt); err != nil {
			t.Fatalf("install rollback trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = env.db.Exec(ctx, `DROP TRIGGER IF EXISTS autiva_activation_fail_audit_trg ON admin_audit_events`)
		_, _ = env.db.Exec(ctx, `DROP FUNCTION IF EXISTS `+functionName+`()`)
	})

	rec = activationPost(t, env.handler, "/api/v1/admin/activation/complete", map[string]string{
		"email": config.AuthorizedAdminEmail, "activationToken": verified.ActivationToken, "password": activationPassword,
	}, csrf, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forced rollback status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var adminCount int
	if err := env.db.Pool().QueryRow(env.ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&adminCount); err != nil || adminCount != 0 {
		t.Fatalf("rollback left an administrator behind: count=%d err=%v", adminCount, err)
	}
	var status string
	if err := env.db.Pool().QueryRow(env.ctx, `SELECT status FROM admin_activation_authorizations`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("rollback changed activation status: status=%s err=%v", status, err)
	}

	if _, err := env.db.Exec(env.ctx, `DROP TRIGGER autiva_activation_fail_audit_trg ON admin_audit_events`); err != nil {
		t.Fatalf("drop rollback trigger: %v", err)
	}
	rec = activationPost(t, env.handler, "/api/v1/admin/activation/complete", map[string]string{
		"email": config.AuthorizedAdminEmail, "activationToken": verified.ActivationToken, "password": activationPassword,
	}, csrf, cookies)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry after rollback status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func TestActivationOriginAndCSRFProtection(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"email": config.AuthorizedAdminEmail})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/activation/request-code", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("X-CSRF-Token", "missing")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(decodeActivationError(t, rec), "forbidden") {
		t.Fatalf("bad origin status = %d, body = %s", rec.Code, rec.Body.String())
	}

	csrf, cookies := activationCSRF(t, env.handler)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/activation/request-code", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", "not-the-cookie-token")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad CSRF status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Sanity check the accepted path still works after the rejected attempts.
	if rec := activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
		map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies); rec.Code != http.StatusAccepted {
		t.Fatalf("valid request status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestActivationRetryAfterReflectsActualLockout(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	csrf, cookies := activationCSRF(t, env.handler)
	request := func() *httptest.ResponseRecorder {
		return activationPost(t, env.handler, "/api/v1/admin/activation/request-code",
			map[string]string{"email": config.AuthorizedAdminEmail}, csrf, cookies)
	}

	if rec := request(); rec.Code != http.StatusAccepted {
		t.Fatalf("first request status = %d: %s", rec.Code, rec.Body.String())
	}
	// The durable 60-second cooldown must report the remaining cooldown.
	cooldown := request()
	if cooldown.Code != http.StatusTooManyRequests {
		t.Fatalf("cooldown status = %d, want 429: %s", cooldown.Code, cooldown.Body.String())
	}
	if got := cooldown.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("cooldown Retry-After = %q, want 60", got)
	}

	// Once the cooldown expires, a third request succeeds but exhausts the
	// three-requests-per-hour limiter and arms a one-hour block.
	env.clock.advance(61 * time.Second)
	if rec := request(); rec.Code != http.StatusAccepted {
		t.Fatalf("post-cooldown request status = %d: %s", rec.Code, rec.Body.String())
	}
	blocked := request()
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("limiter status = %d, want 429: %s", blocked.Code, blocked.Body.String())
	}
	if got := blocked.Header().Get("Retry-After"); got != "3600" {
		t.Fatalf("limiter Retry-After = %q, want 3600", got)
	}
}

func TestActivationResendCapRetryAfterUsesDurableWindow(t *testing.T) {
	env := newActivationEnv(t)
	if _, err := env.svc.Provision(env.ctx, "AutivaShop Administrator"); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}
	provide := func() error {
		return env.svc.RequestCode(env.ctx, config.AuthorizedAdminEmail, "127.0.0.1")
	}
	for i := 0; i < env.cfg.AdminActivation.MaxResends; i++ {
		if i > 0 {
			env.clock.advance(env.cfg.AdminActivation.RequestCooldown + time.Second)
		}
		if err := provide(); err != nil {
			t.Fatalf("code %d request failed: %v", i, err)
		}
	}

	// A fresh limiter isolates the durable PostgreSQL resend cap from the
	// process-local limiter armed by the requests above.
	fresh, err := adminactivation.NewService(env.db.Pool(), env.cfg.AdminActivation, env.sender, testLogger())
	if err != nil {
		t.Fatalf("NewService() failed: %v", err)
	}
	fresh.SetNow(env.clock.now)
	err = fresh.RequestCode(env.ctx, config.AuthorizedAdminEmail, "127.0.0.1")
	if !errors.Is(err, adminactivation.ErrRateLimited) {
		t.Fatalf("resend cap error = %v, want ErrRateLimited", err)
	}
	seconds := adminactivation.RetryAfterSeconds(err)
	if seconds <= 0 || seconds > int64(env.cfg.AdminActivation.ResendWindow/time.Second) {
		t.Fatalf("resend cap Retry-After = %d seconds, want a positive value within the resend window", seconds)
	}
}
