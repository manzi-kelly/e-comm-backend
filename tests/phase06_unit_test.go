package tests

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/customer"
	"autivashop/backend/internal/orders"
)

func TestPhase06CustomerValidation(t *testing.T) {
	phones := []struct {
		raw, e164, display string
		ok                 bool
	}{
		{"0781234567", "+250781234567", "+250 781 234 567", true},
		{"+250 781 234 567", "+250781234567", "+250 781 234 567", true},
		{"00250 781 234 567", "+250781234567", "+250 781 234 567", true},
		{"250 781 234 567", "+250781234567", "+250 781 234 567", true},
		{"781234567", "+250781234567", "+250 781 234 567", true},
		{"+25078123456", "", "", false},
		{"+254781234567", "", "", false},
		{"078123456a", "", "", false},
	}
	for _, tc := range phones {
		e164, display, ok := customer.NormalizeRwandaPhone(tc.raw)
		if ok != tc.ok || e164 != tc.e164 || display != tc.display {
			t.Errorf("phone %q: got %q %q %v", tc.raw, e164, display, ok)
		}
	}
	if !customer.ValidEmail("User.Name@Example.com") || customer.ValidEmail("user..name@example.com") ||
		customer.ValidEmail(strings.Repeat("a", 65)+"@example.com") {
		t.Fatal("email validation mismatch")
	}

	valid := customer.RegistrationInput{
		FullName: "  Aline   Umuhire ", Email: " ALINE@Example.COM ", Phone: "0781234567",
		Password: "Customer-Password-123!", ConfirmPassword: "Customer-Password-123!",
	}
	normalized, fields := customer.ValidateRegistration(valid)
	if len(fields) != 0 || normalized.Name != "Aline Umuhire" || normalized.Email != "aline@example.com" ||
		normalized.PhoneE164 != "+250781234567" || normalized.PhoneDisplay != "+250 781 234 567" {
		t.Fatalf("unexpected normalized registration: %+v %v", normalized, fields)
	}
	weak := valid
	weak.Password = "short"
	if _, fields = customer.ValidateRegistration(weak); len(fields) == 0 || len(fields["password"]) == 0 {
		t.Fatal("weak customer password accepted")
	}
	mismatch := valid
	mismatch.ConfirmPassword = "Different-Password-123!"
	if _, fields = customer.ValidateRegistration(mismatch); len(fields) == 0 || len(fields["confirmPassword"]) == 0 {
		t.Fatal("confirmation mismatch accepted")
	}
}

func TestPhase06OrderValidationRejectsClientPrices(t *testing.T) {
	base := map[string]any{
		"fullName": "Aline Umuhire", "email": "aline@example.com", "phone": "0781234567",
		"province": "Kigali", "district": "Gasabo", "sector": "Remera", "cell": "Rukiri",
		"addressLine": "KG 123 St", "instructions": "Leave at reception",
		"items": []map[string]any{{"productId": "11111111-1111-4111-8111-111111111111", "quantity": 2}},
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	normalized, fields := orders.Decode(raw)
	if len(fields) != 0 || normalized.FullName != "Aline Umuhire" || normalized.Email != "aline@example.com" ||
		normalized.PhoneE164 != "+250781234567" || normalized.PhoneDisplay != "+250 781 234 567" || len(normalized.Fingerprint) != 32 {
		t.Fatalf("valid order decode failed: %+v %v", normalized, fields)
	}
	if normalized.Items[0].VariantID != "" {
		t.Fatal("omitted variant must remain omitted")
	}

	for _, field := range []string{"priceRwf", "subtotalRwf", "totalRwf", "deliveryFeeRwf", "paymentStatus", "reference"} {
		withClientPrice := map[string]any{}
		for k, v := range base {
			withClientPrice[k] = v
		}
		withClientPrice[field] = 1
		body, _ := json.Marshal(withClientPrice)
		if _, fields := orders.Decode(body); len(fields) == 0 {
			t.Errorf("client-controlled field %q was accepted", field)
		}
	}
	for _, body := range []string{
		`{"fullName":"A","fullName":"B"}`,
		`null`,
		`{"items":[]}`,
	} {
		if _, fields := orders.Decode([]byte(body)); len(fields) == 0 {
			t.Errorf("invalid order body accepted: %s", body)
		}
	}
	badVariant := map[string]any{}
	for k, v := range base {
		badVariant[k] = v
	}
	badVariant["items"] = []map[string]any{{"productId": "11111111-1111-4111-8111-111111111111", "variantId": "secret", "quantity": 1}}
	body, _ := json.Marshal(badVariant)
	if _, fields := orders.Decode(body); len(fields) == 0 {
		t.Fatal("unsupported variant accepted")
	}
	baseVariant := map[string]any{}
	for k, v := range base {
		baseVariant[k] = v
	}
	baseVariant["items"] = []map[string]any{{"productId": "11111111-1111-4111-8111-111111111111", "variantId": "base", "quantity": 1}}
	body, _ = json.Marshal(baseVariant)
	if _, fields := orders.Decode(body); len(fields) != 0 {
		t.Fatalf("base variant rejected: %v", fields)
	}
}

func TestPhase06IdempotencyKeyValidation(t *testing.T) {
	for _, key := range []string{"short", "this key has spaces", strings.Repeat("a", 129), ""} {
		if orders.ValidIdempotencyKey(key) {
			t.Errorf("invalid idempotency key accepted: %q", key)
		}
	}
	for _, key := range []string{"checkout_123", "abc.def-01", "ABCDEFGH"} {
		if !orders.ValidIdempotencyKey(key) {
			t.Errorf("valid idempotency key rejected: %q", key)
		}
	}
}

func TestPhase06ConfigAndCookieSeparation(t *testing.T) {
	cfg, err := config.Load(envFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.SessionCookieName == cfg.CustomerAuth.SessionCookieName ||
		cfg.Auth.CSRFCookieName == cfg.CustomerAuth.CSRFCookieName ||
		cfg.CustomerAuth.SessionCookieName == cfg.CustomerAuth.CSRFCookieName {
		t.Fatal("admin and customer cookie names must differ")
	}
	adminCookies := auth.NewCookieConfig(cfg.Auth)
	customerCookies := auth.NewCustomerCookieConfig(cfg)
	if customerCookies.SessionName != "autiva_customer_session" || customerCookies.CSRFName != "autiva_customer_csrf" {
		t.Fatalf("unexpected customer cookie config: %+v", customerCookies)
	}
	if customerCookies.SessionName == adminCookies.SessionName || customerCookies.CSRFName == adminCookies.CSRFName {
		t.Fatal("customer cookie config overlaps admin cookies")
	}
	_, err = config.Load(envFrom(map[string]string{"CUSTOMER_SESSION_COOKIE_NAME": "autiva_admin_session"}))
	if err == nil {
		t.Fatal("customer/admin cookie collision accepted")
	}
	_, err = config.Load(envFrom(map[string]string{"CUSTOMER_SESSION_TTL": "1m"}))
	if err == nil {
		t.Fatal("invalid customer session TTL accepted")
	}
	_, err = config.Load(envFrom(map[string]string{"ORDER_RESERVATION_TTL": "1m"}))
	if err == nil {
		t.Fatal("invalid reservation TTL accepted")
	}
}

func TestPhase06CustomerPrincipalCannotSatisfyAdminContext(t *testing.T) {
	ctx := auth.WithCustomerPrincipal(t.Context(), auth.CustomerPrincipal{ID: "cust-1", SessionID: "sess-1"})
	if p, ok := auth.CustomerPrincipalFromContext(ctx); !ok || p.ID != "cust-1" {
		t.Fatal("customer principal missing")
	}
	if _, ok := auth.PrincipalFromContext(ctx); ok {
		t.Fatal("customer principal leaked into admin context")
	}
	ctx2 := auth.WithCustomerSession(ctx, "sess-1")
	if id, ok := auth.CustomerSessionFromContext(ctx2); !ok || id != "sess-1" {
		t.Fatal("customer session context missing")
	}
	adminCtx := auth.WithPrincipal(t.Context(), auth.AdminPrincipal{ID: "admin-1", Role: "super_admin", IsActive: true})
	if _, ok := auth.CustomerPrincipalFromContext(adminCtx); ok {
		t.Fatal("admin principal leaked into customer context")
	}
}

func TestPhase06CustomerProfileDoesNotExposeSecrets(t *testing.T) {
	account := customer.Customer{ID: "id", Name: "Aline", Email: "aline@example.com", PhoneDisplay: "+250 781 234 567", PhoneE164: "+250781234567", PasswordHash: "$argon2id$secret", Status: "active", CreatedAt: time.Now()}
	profile := account.Public()
	b, _ := json.Marshal(profile)
	if strings.Contains(string(b), "password") || strings.Contains(string(b), "argon2id") || strings.Contains(string(b), "token") {
		t.Fatalf("profile leaked sensitive data: %s", b)
	}
	if !strings.Contains(string(b), `"fullName":"Aline"`) {
		t.Fatalf("profile missing fullName: %s", b)
	}
}
