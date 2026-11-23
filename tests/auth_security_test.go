package tests

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autivashop/backend/internal/auth"
)

func TestPasswordHashAndVerify(t *testing.T) {
	password := "Str0ng!AdminPassw0rd"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if hash == "" || strings.Contains(hash, password) {
		t.Fatal("hash must not be empty or contain plaintext password")
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("unexpected hash format: %q", hash)
	}
	if err := auth.VerifyPassword(password, hash); err != nil {
		t.Fatalf("VerifyPassword failed for valid password: %v", err)
	}
	if err := auth.VerifyPassword("wrong-password-0000", hash); err == nil {
		t.Fatal("VerifyPassword accepted wrong password")
	}
	hash2, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("second HashPassword failed: %v", err)
	}
	if hash == hash2 {
		t.Fatal("salts should make hashes unique")
	}
}

func TestPasswordStrengthValidation(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"too short", "short123", true},
		{"all same", "aaaaaaaaaaaa", true},
		{"whitespace only padding", "   aaaaaaa   ", true},
		{"valid long", "Valid-Admin-Password-123", false},
		{"too long", strings.Repeat("a", 129), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := auth.ValidatePasswordStrength(tc.password)
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	if err := auth.VerifyPassword(strings.Repeat("x", 5000), "$argon2id$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("oversized password should fail safely, got %v", err)
	}
}

func TestTokenGenerationAndHashing(t *testing.T) {
	tok1, err := auth.GenerateToken(32)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	tok2, err := auth.GenerateToken(32)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if tok1 == "" || tok1 == tok2 {
		t.Fatal("tokens must be non-empty and unique")
	}
	h1 := auth.HashToken(tok1)
	h2 := auth.HashToken(tok1)
	if len(h1) != 32 || !auth.TokensEqual(h1, h2) {
		t.Fatal("token hash must be stable and comparable")
	}
	if auth.TokensEqual(h1, auth.HashToken(tok2)) {
		t.Fatal("different tokens must not have equal hashes")
	}
}

func TestCSRFHelpers(t *testing.T) {
	if auth.RequiresCSRF(http.MethodGet) || !auth.RequiresCSRF(http.MethodPost) {
		t.Fatal("unexpected CSRF method policy")
	}
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	if !auth.ValidateOrigin(req, []string{"http://localhost:3000"}) {
		t.Fatal("expected allowed origin")
	}
	if auth.ValidateOrigin(req, []string{"http://127.0.0.1:3000"}) {
		t.Fatal("unexpected origin match")
	}
	req2 := httptest.NewRequest(http.MethodPost, "/x", nil)
	req2.Header.Set("Referer", "http://localhost:3000/admin")
	if !auth.ValidateOrigin(req2, []string{"http://localhost:3000"}) {
		t.Fatal("expected allowed referer origin")
	}
	req3 := httptest.NewRequest(http.MethodPost, "/x", nil)
	req3.AddCookie(&http.Cookie{Name: "csrf", Value: "token123"})
	req3.Header.Set("X-CSRF-Token", "token123")
	if !auth.ValidCSRFToken(req3, "csrf", "X-CSRF-Token") {
		t.Fatal("expected valid csrf token")
	}
	req3.Header.Set("X-CSRF-Token", "wrong")
	if auth.ValidCSRFToken(req3, "csrf", "X-CSRF-Token") {
		t.Fatal("expected csrf mismatch")
	}
}

func TestLoginLimiter(t *testing.T) {
	l := auth.NewLoginLimiter(2, time.Minute)
	ok, _ := l.Allow("ip:1")
	if !ok {
		t.Fatal("first attempt should be allowed")
	}
	l.RecordFailure("ip:1")
	ok, _ = l.Allow("ip:1")
	if !ok {
		t.Fatal("second attempt should be allowed before threshold")
	}
	l.RecordFailure("ip:1")
	ok, retry := l.Allow("ip:1")
	if ok || retry <= 0 {
		t.Fatal("expected temporary throttling after failures")
	}
	l.RecordSuccess("ip:1")
	ok, _ = l.Allow("ip:1")
	if !ok {
		t.Fatal("success should reset throttling state")
	}
	_ = auth.ClientIP(httptest.NewRequest(http.MethodPost, "/login", nil))
}
