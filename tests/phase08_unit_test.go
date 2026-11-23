package tests

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"autivashop/backend/internal/config"
	"autivashop/backend/internal/payment"
)

func TestPhase08PaymentConfigurationBoundaries(t *testing.T) {
	dev, err := config.Load(envFrom(map[string]string{"APP_ENV": "development"}))
	if err != nil {
		t.Fatal(err)
	}
	if dev.Payment.Provider != config.PaymentProviderMock || !dev.Payment.IsMock ||
		dev.Payment.Environment != config.EnvDevelopment {
		t.Fatalf("unexpected development payment config: %+v", dev.Payment)
	}

	testingEnv, err := config.Load(envFrom(map[string]string{
		"APP_ENV":          "test",
		"PAYMENT_PROVIDER": config.PaymentProviderXendlyPay,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if testingEnv.Payment.Provider != config.PaymentProviderXendlyPay || testingEnv.Payment.IsMock ||
		testingEnv.Payment.Environment != "test" {
		t.Fatalf("unexpected test xendlypay boundary: %+v", testingEnv.Payment)
	}

	if _, err := config.Load(envFrom(map[string]string{"PAYMENT_PROVIDER": "paypal"})); err == nil {
		t.Fatal("unknown payment provider was accepted")
	}

	productionBase := map[string]string{
		"APP_ENV":      "production",
		"API_HOST":     "0.0.0.0",
		"CORS_ORIGINS": "https://shop.example.com",
		"DATABASE_URL": "postgres://app:pw@db.example.com:5432/autivashop?sslmode=verify-full",
	}
	for _, provider := range []string{"", config.PaymentProviderMock, config.PaymentProviderXendlyPay} {
		env := map[string]string{}
		for key, value := range productionBase {
			env[key] = value
		}
		if provider != "" {
			env["PAYMENT_PROVIDER"] = provider
		}
		if _, err := config.Load(envFrom(env)); err == nil {
			t.Fatalf("production accepted provider %q", provider)
		}
	}
}

func TestPhase08RwandaMobileMoneyNumbers(t *testing.T) {
	valid := []struct {
		raw, msisdn, e164, network string
	}{
		{"0781234567", "250781234567", "+250781234567", "mtn"},
		{"+250 781-234-567", "250781234567", "+250781234567", "mtn"},
		{"250791234567", "250791234567", "+250791234567", "mtn"},
		{"0721234567", "250721234567", "+250721234567", "airtel"},
		{"0731234567", "250731234567", "+250731234567", "airtel"},
	}
	for _, tc := range valid {
		msisdn, e164, network, err := payment.NormalizeRwandaMSISDN(tc.raw)
		if err != nil || msisdn != tc.msisdn || e164 != tc.e164 || network != tc.network {
			t.Fatalf("normalize %q = %q,%q,%q,%v", tc.raw, msisdn, e164, network, err)
		}
	}

	for _, raw := range []string{
		"", "1234", "781234567", "25078123456", "2507812345678",
		"+254781234567", "250741234567", "25078123456a", "0781234567#4",
	} {
		if _, _, _, err := payment.NormalizeRwandaMSISDN(raw); !errors.Is(err, payment.ErrInvalidPhone) {
			t.Fatalf("accepted invalid phone %q", raw)
		}
	}
}

func TestPhase08MockProviderIsLocalAndDeterministic(t *testing.T) {
	mock := payment.NewMockPaymentProvider("test")
	fixed := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	mock.SetNow(func() time.Time { return fixed })
	var provider payment.Provider = mock
	if provider.Name() != "mock" || provider.Environment() != "test" || !provider.IsMock() {
		t.Fatalf("unexpected mock provider identity: %s %s %v",
			provider.Name(), provider.Environment(), provider.IsMock())
	}

	req := payment.InitiateRequest{
		OrderID:                "11111111-1111-1111-1111-111111111111",
		OrderReference:         "AV-20261123-ABCDEFGHJKLM",
		AmountRWF:              125000,
		Currency:               "RWF",
		MSISDN:                 "250781234567",
		PhoneE164:              "+250781234567",
		Network:                "mtn",
		ProviderIdempotencyKey: "av-aaaaaaaaaaaaaaaaaaaaaaaa",
		Description:            "Order AV-20261123-ABCDEFGHJKLM",
	}
	result, err := provider.Initiate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != payment.StatePending || result.PromptStatus != payment.PromptSimulatedPending {
		t.Fatalf("mock initiation must stay pending: %+v", result)
	}
	if !strings.HasPrefix(result.ProviderReference, "MOCK-") || result.Method != "prompt" ||
		result.Mode != "mock" || !result.CreatedAt.Equal(fixed) {
		t.Fatalf("unexpected mock result: %+v", result)
	}
	replay, err := provider.Initiate(context.Background(), req)
	if err != nil || replay.ProviderReference != result.ProviderReference {
		t.Fatalf("mock reference is not deterministic: %+v %v", replay, err)
	}

	bad := req
	bad.Network = ""
	if _, err := provider.Initiate(context.Background(), bad); !errors.Is(err, payment.ErrInvalidRequest) {
		t.Fatalf("invalid mock request was accepted: %v", err)
	}

	simulator, ok := provider.(payment.Simulator)
	if !ok {
		t.Fatal("mock provider must implement the simulator capability")
	}
	cases := []struct {
		outcome, state, prompt, event, failure string
	}{
		{payment.OutcomeApprove, payment.StateSucceeded, payment.PromptSimulatedApproved, "payment.succeeded", ""},
		{payment.OutcomeReject, payment.StateFailed, payment.PromptSimulatedRejected, "payment.failed", "SIMULATED_REJECTED"},
		{payment.OutcomeTimeout, payment.StateFailed, payment.PromptSimulatedTimeout, "payment.failed", "SIMULATED_TIMEOUT"},
	}
	for _, tc := range cases {
		simReq := payment.SimulationRequest{
			AttemptID:      "22222222-2222-2222-2222-222222222222",
			OrderReference: req.OrderReference,
			Outcome:        tc.outcome,
			AmountRWF:      req.AmountRWF,
		}
		sim, err := simulator.Simulate(context.Background(), simReq)
		if err != nil {
			t.Fatal(err)
		}
		if sim.State != tc.state || sim.PromptStatus != tc.prompt || sim.EventType != tc.event ||
			sim.FailureCode != tc.failure || sim.DeliveryID != "sim-"+simReq.AttemptID+"-"+tc.outcome {
			t.Fatalf("unexpected simulation %s: %+v", tc.outcome, sim)
		}
		if sim.Payload["simulated"] != true {
			t.Fatalf("simulation payload did not declare itself simulated: %+v", sim.Payload)
		}
		again, err := simulator.Simulate(context.Background(), simReq)
		if err != nil || again.DeliveryID != sim.DeliveryID {
			t.Fatalf("simulation delivery is not deterministic: %+v %v", again, err)
		}
	}
	if _, err := simulator.Simulate(context.Background(), payment.SimulationRequest{
		AttemptID: "x", OrderReference: req.OrderReference, Outcome: "pin", AmountRWF: 1,
	}); err == nil {
		t.Fatal("invalid simulation outcome was accepted")
	}
	if !strings.Contains(payment.PromptMessage, "No phone message") {
		t.Fatalf("prompt message claims delivery: %q", payment.PromptMessage)
	}
}

func TestPhase08XendlyPayBoundaryNeverSucceeds(t *testing.T) {
	var provider payment.Provider = payment.NewXendlyPayProvider()
	if provider.Name() != "xendlypay" || provider.IsMock() {
		t.Fatalf("unexpected live boundary identity: %s %v", provider.Name(), provider.IsMock())
	}
	result, err := provider.Initiate(context.Background(), payment.InitiateRequest{
		OrderID:   "11111111-1111-1111-1111-111111111111",
		AmountRWF: 100, Currency: "RWF",
	})
	if !errors.Is(err, payment.ErrLiveNotImplemented) {
		t.Fatalf("live boundary returned %v instead of not-implemented", err)
	}
	if result.State != "" || result.ProviderReference != "" || result.PromptStatus != "" {
		t.Fatalf("live boundary returned a simulated success: %+v", result)
	}
	if _, ok := provider.(payment.Simulator); ok {
		t.Fatal("live boundary must not be simulatable")
	}
	if _, ok := provider.(payment.Retriever); ok {
		t.Fatal("retrieval boundary must stay unimplemented in Phase 08")
	}
}

func TestPhase08WebhookSignatureDesignOnly(t *testing.T) {
	secret := []byte("local-design-only-secret")
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"type":"payment.succeeded","data":{"amount":100}}`)
	sign := func(timestamp int64, payload []byte) string {
		mac := hmac.New(sha256.New, secret)
		_, _ = fmt.Fprintf(mac, "%d.", timestamp)
		_, _ = mac.Write(payload)
		return hex.EncodeToString(mac.Sum(nil))
	}
	validHeader := fmt.Sprintf("t=%d,v1=%s", now.Unix(), sign(now.Unix(), body))
	if err := payment.VerifyWebhookSignature(body, validHeader, secret, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := payment.VerifyWebhookSignature([]byte(`{"type":"payment.failed"}`), validHeader, secret, now); !errors.Is(err, payment.ErrInvalidWebhookSignature) {
		t.Fatalf("tampered body accepted: %v", err)
	}
	stale := now.Add(-payment.WebhookFreshnessWindow - time.Second).Unix()
	staleHeader := fmt.Sprintf("t=%d,v1=%s", stale, sign(stale, body))
	if err := payment.VerifyWebhookSignature(body, staleHeader, secret, now); !errors.Is(err, payment.ErrStaleWebhook) {
		t.Fatalf("stale webhook accepted: %v", err)
	}
	if err := payment.VerifyWebhookSignature(body, "v1="+sign(now.Unix(), body), secret, now); !errors.Is(err, payment.ErrInvalidWebhookSignature) {
		t.Fatalf("missing timestamp accepted: %v", err)
	}
}
