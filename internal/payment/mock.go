package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var errInvalidSimulation = errors.New("invalid mock simulation request")

// MockPaymentProvider is a pure local simulator. It performs no network I/O,
// stores no credentials, sends no phone prompt and handles no real money.
// References and simulated events are deterministic for a given request.
type MockPaymentProvider struct {
	environment string
	now         func() time.Time
}

func NewMockPaymentProvider(environment string) *MockPaymentProvider {
	return &MockPaymentProvider{environment: environment, now: time.Now}
}

// SetNow is a test hook for deterministic event timestamps.
func (p *MockPaymentProvider) SetNow(now func() time.Time) { p.now = now }

func (p *MockPaymentProvider) Name() string        { return "mock" }
func (p *MockPaymentProvider) Environment() string { return p.environment }
func (p *MockPaymentProvider) IsMock() bool        { return true }

func mockReference(key string) string {
	sum := sha256.Sum256([]byte("autivashop-mock|" + key))
	return "MOCK-" + strings.ToUpper(hex.EncodeToString(sum[:8]))
}

// Initiate always returns a pending, simulated prompt result.
func (p *MockPaymentProvider) Initiate(_ context.Context, req InitiateRequest) (InitiateResult, error) {
	if req.OrderID == "" || req.OrderReference == "" || req.AmountRWF < 1 ||
		req.Currency != "RWF" || req.ProviderIdempotencyKey == "" ||
		(req.Network != "mtn" && req.Network != "airtel") {
		return InitiateResult{}, ErrInvalidRequest
	}
	return InitiateResult{
		ProviderReference: mockReference(req.ProviderIdempotencyKey),
		State:             StatePending,
		PromptStatus:      PromptSimulatedPending,
		Method:            "prompt",
		Mode:              "mock",
		CreatedAt:         p.now().UTC(),
	}, nil
}

// Simulate records a controlled local outcome. The same attempt/outcome pair
// always produces the same delivery ID, enabling idempotent replay.
func (p *MockPaymentProvider) Simulate(_ context.Context, req SimulationRequest) (SimulationResult, error) {
	if req.AttemptID == "" || req.OrderReference == "" || req.AmountRWF < 1 {
		return SimulationResult{}, errInvalidSimulation
	}
	now := p.now().UTC()
	result := SimulationResult{OccurredAt: now, Payload: map[string]any{
		"provider": "mock", "environment": p.environment, "orderReference": req.OrderReference,
		"amount": req.AmountRWF, "currency": "RWF", "simulated": true,
	}}
	switch req.Outcome {
	case OutcomeApprove:
		result.State = StateSucceeded
		result.PromptStatus = PromptSimulatedApproved
		result.EventType = "payment.succeeded"
	case OutcomeReject:
		result.State = StateFailed
		result.PromptStatus = PromptSimulatedRejected
		result.EventType = "payment.failed"
		result.FailureCode = "SIMULATED_REJECTED"
		result.FailureMessage = "Simulated customer rejection; no phone prompt was sent."
	case OutcomeTimeout:
		result.State = StateFailed
		result.PromptStatus = PromptSimulatedTimeout
		result.EventType = "payment.failed"
		result.FailureCode = "SIMULATED_TIMEOUT"
		result.FailureMessage = "Simulated prompt timeout; no phone prompt was sent."
	default:
		return SimulationResult{}, errInvalidSimulation
	}
	result.DeliveryID = "sim-" + req.AttemptID + "-" + req.Outcome
	result.Payload["state"] = result.State
	result.Payload["eventType"] = result.EventType
	result.Payload["deliveryId"] = result.DeliveryID
	return result, nil
}
