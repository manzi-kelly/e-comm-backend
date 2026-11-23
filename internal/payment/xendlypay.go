package payment

import "context"

// Verified XendlyPay contracts (https://xendlypay.com/docs) are transcribed
// here as typed documentation for a FUTURE integration. This file performs no
// networking, reads no credentials, and never returns a simulated success.
// A later phase must independently verify credentials, TLS, webhook secrets
// and payments before enabling this provider in production.

const (
	XendlyBaseURL           = "https://api.xendlypay.com"
	XendlyInitiatePath      = "/v1/payments"
	XendlyRetrievePath      = "/v1/payments/%s"
	XendlyAuthorization     = "Authorization: Bearer <key>"
	XendlyAPIKeyHeader      = "X-Api-Key"
	XendlyIdempotencyHeader = "Idempotency-Key"
	XendlySignatureHeader   = "X-Xendly-Signature"
	XendlyEventHeader       = "X-Xendly-Event"
	XendlyDeliveryHeader    = "X-Xendly-Delivery"
	XendlyMinAmountRWF      = 100
)

// XendlyInitiateRequest mirrors POST /v1/payments. Amount is an integer in RWF;
// the server chooses the reference and idempotency key from its own order data.
type XendlyInitiateRequest struct {
	Amount      int64             `json:"amount"`
	Currency    string            `json:"currency"`
	MSISDN      string            `json:"msisdn"`
	Description string            `json:"description,omitempty"`
	Reference   string            `json:"reference,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Method      string            `json:"method,omitempty"`
}

// XendlyEnvelope is the success wrapper used by the documented API.
type XendlyEnvelope struct {
	Success   bool          `json:"success"`
	Payload   XendlyPayment `json:"payload"`
	Timestamp string        `json:"timestamp"`
}

// XendlyErrorEnvelope is the documented error shape; callers must check the
// HTTP status first, then the message.
type XendlyErrorEnvelope struct {
	Status    string `json:"status"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// XendlyPayment is the authoritative payment object returned by initiation,
// retrieval and payment.* webhooks. Final states are succeeded/failed/cancelled;
// a network timeout becomes failed, not expired.
type XendlyPayment struct {
	XendlyReference string         `json:"xendly_reference"`
	Reference       string         `json:"reference"`
	Kind            string         `json:"kind"`
	Status          string         `json:"status"`
	Amount          int64          `json:"amount"`
	Fee             int64          `json:"fee"`
	Currency        string         `json:"currency"`
	MSISDN          string         `json:"msisdn"`
	Network         string         `json:"network"`
	Method          string         `json:"method"`
	Mode            string         `json:"mode"`
	Description     string         `json:"description,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	FailureCode     *string        `json:"failure_code,omitempty"`
	FailureMessage  *string        `json:"failure_message,omitempty"`
	CreatedAt       string         `json:"created_at"`
	UpdatedAt       string         `json:"updated_at,omitempty"`
	CompletedAt     *string        `json:"completed_at,omitempty"`
}

// XendlyWebhookEvent is the future signed webhook payload. Every delivery
// includes the whole payment object so handlers and reconciliation can share
// the same validation and state-transition code.
type XendlyWebhookEvent struct {
	Type string        `json:"type"`
	Data XendlyPayment `json:"data"`
}

// XendlyPayProvider is an inert integration boundary. It deliberately has no
// HTTP client, no key fields and no live initiation. It must never be used to
// mark an order paid or to report revenue.
type XendlyPayProvider struct{}

func NewXendlyPayProvider() *XendlyPayProvider { return &XendlyPayProvider{} }

func (p *XendlyPayProvider) Name() string        { return "xendlypay" }
func (p *XendlyPayProvider) Environment() string { return "test" }
func (p *XendlyPayProvider) IsMock() bool        { return false }

func (p *XendlyPayProvider) Initiate(_ context.Context, _ InitiateRequest) (InitiateResult, error) {
	return InitiateResult{}, ErrLiveNotImplemented
}
