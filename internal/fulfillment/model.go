// Package fulfillment implements the backend-authoritative, Admin-controlled
// fulfillment state machine. It never mutates products, prices, payment attempts
// or customer identities, and it never infers locations or delivery times.
package fulfillment

import (
	"errors"
	"time"
)

const (
	StatusPending          = "pending"
	StatusProcessing       = "processing"
	StatusReadyForDelivery = "ready_for_delivery"
	StatusOutForDelivery   = "out_for_delivery"
	StatusDelivered        = "delivered"
	LegacyStockUnavailable = "payment_received_stock_unavailable"

	SettlementNotPaid      = "not_paid"
	SettlementSimulated    = "simulated_dev_test"
	SettlementLiveVerified = "live_verified"

	EventStartProcessing = "fulfillment.processing.started"
	EventMarkReady       = "fulfillment.ready_for_delivery.marked"
	EventDispatch        = "fulfillment.out_for_delivery.dispatched"
	EventConfirmDelivery = "fulfillment.delivered.confirmed"

	ActorAdmin  = "admin"
	ActorSystem = "system"

	ConfirmationMethod = "admin_manual"
	ConfirmationSource = "admin_orders_api"
	CountryRW          = "RW"
)

var (
	ErrValidation           = errors.New("fulfillment input failed validation")
	ErrNotFound             = errors.New("fulfillment record not found")
	ErrInvalidTransition    = errors.New("fulfillment state transition is not allowed")
	ErrStaleVersion         = errors.New("fulfillment version is stale")
	ErrIdempotencyConflict  = errors.New("fulfillment idempotency key reused with a different request")
	ErrPaymentNotEligible   = errors.New("payment is not eligible for fulfillment")
	ErrAllocationUnresolved = errors.New("inventory allocation is unresolved")
	ErrStockUnavailable     = errors.New("stock is unavailable for fulfillment")
	ErrForbidden            = errors.New("fulfillment action is not permitted")
)

// Actor is supplied by the authenticated Admin request context. A customer
// principal can never construct one of these.
type Actor struct {
	AdminID   string
	SessionID string
	Email     string
	Role      string
	IP        string
	UserAgent string
}

// Command is the only mutation input. Status, timestamps, settlement mode and
// delivery metadata are never accepted from a client.
type Command struct {
	OrderID         string
	Event           string
	ExpectedVersion int64
	Note            string
	IdempotencyKey  string
}

// LocalDelivery is copied from the existing Rwanda order snapshot. It contains
// no GPS coordinates, courier identifiers, ETA or invented landmark.
type LocalDelivery struct {
	Country      string  `json:"country"`
	Province     string  `json:"province"`
	District     string  `json:"district"`
	Sector       string  `json:"sector"`
	Cell         *string `json:"cell"`
	AddressLine  string  `json:"addressLine"`
	Instructions *string `json:"instructions"`
	ContactName  string  `json:"contactName"`
	PhoneMasked  string  `json:"phoneMasked"`
}

// Confirmation records how delivery was confirmed. There is no signature,
// photo, GPS proof or third-party courier callback in this phase.
type Confirmation struct {
	ConfirmedAt time.Time `json:"confirmedAt"`
	Method      string    `json:"method"`
	Source      string    `json:"source"`
	Note        *string   `json:"note,omitempty"`
	ConfirmedBy string    `json:"confirmedBy"`
}

// State is the Admin/customer-safe current projection.
type State struct {
	OrderID         string        `json:"orderId"`
	OrderReference  string        `json:"orderReference"`
	Status          string        `json:"status"`
	Version         int64         `json:"version"`
	SettlementMode  string        `json:"settlementMode"`
	CanTransitionTo []string      `json:"canTransitionTo"`
	LocalDelivery   LocalDelivery `json:"localDelivery"`
	Confirmation    *Confirmation `json:"confirmation,omitempty"`
	UpdatedAt       time.Time     `json:"updatedAt"`
}

// AdminTransition exposes actor attribution and internal notes to Admin only.
type AdminTransition struct {
	ID             string    `json:"id"`
	FromStatus     string    `json:"fromStatus"`
	ToStatus       string    `json:"toStatus"`
	Version        int64     `json:"version"`
	EventType      string    `json:"eventType"`
	ActorType      string    `json:"actorType"`
	ActorAdminID   *string   `json:"actorAdminId,omitempty"`
	ActorEmail     *string   `json:"actorEmail,omitempty"`
	ActorRole      *string   `json:"actorRole,omitempty"`
	Note           *string   `json:"note,omitempty"`
	IdempotencyKey string    `json:"idempotencyKey"`
	OccurredAt     time.Time `json:"occurredAt"`
}

// CustomerTransition is deliberately narrower: no internal note, admin email,
// session identifier, IP or user agent is ever returned to a customer.
type CustomerTransition struct {
	FromStatus string    `json:"fromStatus"`
	ToStatus   string    `json:"toStatus"`
	EventType  string    `json:"eventType"`
	OccurredAt time.Time `json:"occurredAt"`
}

// Conflict is a computed, read-only blocker. It never repairs data.
type Conflict struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CustomerView is the authenticated owner-only progress projection.
type CustomerView struct {
	State         State                `json:"state"`
	History       []CustomerTransition `json:"history"`
	PaymentStatus string               `json:"paymentStatus"`
	OrderStatus   string               `json:"orderStatus"`
}

// TransitionResult reports whether a request was a replay of a previously
// committed idempotency key. Replays never create a second audit row.
type TransitionResult struct {
	State    State `json:"state"`
	Replayed bool  `json:"replayed"`
}

var transitions = map[string]struct {
	from  string
	to    string
	event string
}{
	EventStartProcessing: {StatusPending, StatusProcessing, EventStartProcessing},
	EventMarkReady:       {StatusProcessing, StatusReadyForDelivery, EventMarkReady},
	EventDispatch:        {StatusReadyForDelivery, StatusOutForDelivery, EventDispatch},
	EventConfirmDelivery: {StatusOutForDelivery, StatusDelivered, EventConfirmDelivery},
}

var nextEvents = map[string][]string{
	StatusPending:          {EventStartProcessing},
	StatusProcessing:       {EventMarkReady},
	StatusReadyForDelivery: {EventDispatch},
	StatusOutForDelivery:   {EventConfirmDelivery},
	StatusDelivered:        {},
}

// ValidStatus reports whether a value is a real fulfillment state (not a
// legacy order-level blocker).
func ValidStatus(value string) bool {
	switch value {
	case StatusPending, StatusProcessing, StatusReadyForDelivery, StatusOutForDelivery, StatusDelivered:
		return true
	default:
		return false
	}
}

func eventForStatus(status string) string {
	if events := nextEvents[status]; len(events) == 1 {
		return events[0]
	}
	return ""
}
