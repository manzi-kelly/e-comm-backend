// Package payment separates order/payment business logic from provider
// execution. In Phase 08 the only executable provider is a local simulator;
// XendlyPay is a typed future boundary with no networking and no credentials.
package payment

import (
	"context"
	"errors"
	"time"
)

// Provider-neutral payment states. XendlyPay maps its final states directly;
// a timed-out prompt is reported as failed by the provider, not as a separate
// "expired" state that its API contract does not provide.
const (
	StatePending   = "pending"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Prompt states are simulation-only. They are never a claim that a real phone
// received a message or that a customer entered a PIN.
const (
	PromptNotApplicable     = "not_applicable"
	PromptSimulatedPending  = "simulated_pending"
	PromptSimulatedApproved = "simulated_approved"
	PromptSimulatedRejected = "simulated_rejected"
	PromptSimulatedTimeout  = "simulated_timeout"
)

const (
	OutcomeApprove = "approve"
	OutcomeReject  = "reject"
	OutcomeTimeout = "timeout"
)

var (
	ErrProviderUnavailable = errors.New("payment provider is not available")
	ErrLiveNotImplemented  = errors.New("live payment provider is not implemented")
	ErrInvalidPhone        = errors.New("invalid Rwanda mobile money phone")
	ErrInvalidRequest      = errors.New("invalid payment request")
)

// InitiateRequest contains only server-authoritative values derived from a
// persisted order. No client-supplied price is ever included.
type InitiateRequest struct {
	OrderID                string
	OrderReference         string
	AmountRWF              int64
	Currency               string
	MSISDN                 string
	PhoneE164              string
	Network                string
	IdempotencyKey         string
	ProviderIdempotencyKey string
	Description            string
	Metadata               map[string]string
}

// InitiateResult is the provider's immediate answer. XendlyPay's 201 response
// is pending; pending is never a paid signal.
type InitiateResult struct {
	ProviderReference string
	State             string
	PromptStatus      string
	Method            string
	Mode              string
	CreatedAt         time.Time
}

// SimulationRequest is accepted only by the mock provider in development/test.
type SimulationRequest struct {
	AttemptID         string
	OrderReference    string
	Outcome           string
	AmountRWF         int64
	ProviderReference string
}

// SimulationResult describes a deterministic, local event. It is stored in the
// same transaction as the payment state transition.
type SimulationResult struct {
	DeliveryID     string
	EventType      string
	State          string
	PromptStatus   string
	FailureCode    string
	FailureMessage string
	OccurredAt     time.Time
	Payload        map[string]any
}

// Provider is the execution boundary. Business logic depends only on this
// interface, so a future verified XendlyPay implementation can replace the
// mock without rewriting order or payment lifecycle rules.
type Provider interface {
	Name() string
	Environment() string
	IsMock() bool
	Initiate(ctx context.Context, req InitiateRequest) (InitiateResult, error)
}

// Simulator is a separate capability. It is implemented only by the mock
// provider and is never exposed in production.
type Simulator interface {
	Simulate(ctx context.Context, req SimulationRequest) (SimulationResult, error)
}

// Retriever is reserved for future provider-reference reconciliation. It is
// intentionally not implemented in Phase 08.
type Retriever interface {
	Retrieve(ctx context.Context, providerReference string) (ProviderPayment, error)
}

// ProviderPayment is the verified future contract of GET /v1/payments/{reference}.
type ProviderPayment struct {
	XendlyReference string
	Reference       string
	Kind            string
	Status          string
	Amount          int64
	Currency        string
	MSISDN          string
	Network         string
	Mode            string
	CreatedAt       time.Time
	CompletedAt     *time.Time
}
