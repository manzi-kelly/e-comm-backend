// Package orders implements authoritative, transactional order creation from
// published Rwanda-stock products. Client prices are never accepted.
package orders

import (
	"errors"
	"time"
)

const (
	PaymentPending     = "pending_payment"
	FulfillmentPending = "pending"
	CountryRW          = "RW"
	CurrencyRWF        = "RWF"
	FeeUnavailable     = "unavailable"
	FullPaymentTerm    = "full_amount"
	VariantUnsupported = true // backend 0003 has no product variants
)

var (
	ErrValidation          = errors.New("order input failed validation")
	ErrNotFound            = errors.New("order not found")
	ErrProductNotFound     = errors.New("product not available for ordering")
	ErrStockUnavailable    = errors.New("requested quantity is not available")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with a different request")
	ErrConflict            = errors.New("order request conflicts with a concurrent submission")
)

// Input mirrors the storefront checkout fields. Prices, subtotals, totals,
// payment status and order references are deliberately absent.
type Input struct {
	FullName     string      `json:"fullName"`
	Email        string      `json:"email"`
	Phone        string      `json:"phone"`
	Province     string      `json:"province"`
	District     string      `json:"district"`
	Sector       string      `json:"sector"`
	Cell         string      `json:"cell"`
	AddressLine  string      `json:"addressLine"`
	Instructions string      `json:"instructions"`
	Items        []InputItem `json:"items"`
}

type InputItem struct {
	ProductID string `json:"productId"`
	VariantID string `json:"variantId,omitempty"`
	Quantity  int    `json:"quantity"`
}

// NormalizedInput is validated server-side input plus a fingerprint used for
// idempotent replay detection. It is never sent by a client.
type NormalizedInput struct {
	FullName     string
	Email        string
	PhoneE164    string
	PhoneDisplay string
	Province     string
	District     string
	Sector       string
	Cell         string
	Instructions string
	AddressLine  string
	Items        []InputItem
	Fingerprint  []byte
}

type ContactSnapshot struct {
	FullName  string `json:"fullName"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	PhoneE164 string `json:"phoneE164"`
}

type DeliverySnapshot struct {
	Country      string  `json:"country"`
	Province     string  `json:"province"`
	District     string  `json:"district"`
	Sector       string  `json:"sector"`
	Cell         *string `json:"cell"`
	AddressLine  string  `json:"addressLine"`
	Instructions *string `json:"instructions"`
}

type ItemSnapshot struct {
	ID           string   `json:"id"`
	ProductID    string   `json:"productId"`
	SKU          string   `json:"sku"`
	Name         ItemName `json:"name"`
	Condition    string   `json:"condition"`
	VariantID    *string  `json:"variantId"`
	Quantity     int      `json:"quantity"`
	UnitPriceRWF int64    `json:"unitPriceRwf"`
	LineTotalRWF int64    `json:"lineTotalRwf"`
}

type ItemName struct {
	EN string  `json:"en"`
	RW *string `json:"rw"`
}

type Reservation struct {
	Status        string     `json:"status"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	ReleasePolicy string     `json:"releasePolicy"`
}

type TimelineEntry struct {
	Step string     `json:"step"`
	At   *time.Time `json:"at"`
}

// Order is the authoritative summary returned after creation and on reads.
type Order struct {
	ID                string           `json:"id"`
	Reference         string           `json:"reference"`
	OrderedAt         time.Time        `json:"orderedAt"`
	Status            string           `json:"status"`
	PaymentStatus     string           `json:"paymentStatus"`
	Currency          string           `json:"currency"`
	SubtotalRWF       int64            `json:"subtotalRwf"`
	DeliveryFeeRWF    *int64           `json:"deliveryFeeRwf"`
	DeliveryFeeStatus string           `json:"deliveryFeeStatus"`
	TotalRWF          int64            `json:"totalRwf"`
	AmountDueRWF      int64            `json:"amountDueRwf"`
	PaidRWF           int64            `json:"paidRwf"`
	PaymentTerm       string           `json:"paymentTerm"`
	Customer          ContactSnapshot  `json:"customer"`
	Delivery          DeliverySnapshot `json:"delivery"`
	Items             []ItemSnapshot   `json:"items"`
	Timeline          []TimelineEntry  `json:"timeline"`
	Reservation       Reservation      `json:"reservation"`
}

// Summary is the owner-scoped order-history projection.
type Summary struct {
	ID            string    `json:"id"`
	Reference     string    `json:"reference"`
	OrderedAt     time.Time `json:"orderedAt"`
	Status        string    `json:"status"`
	PaymentStatus string    `json:"paymentStatus"`
	TotalRWF      int64     `json:"totalRwf"`
	ItemCount     int       `json:"itemCount"`
}
