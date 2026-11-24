// Package adminreport provides read-only PostgreSQL projections for the
// authenticated Admin Orders and Dashboard surfaces. It never mutates orders,
// inventory, payment attempts or fulfillment state.
package adminreport

import (
	"errors"
	"time"

	"autivashop/backend/internal/fulfillment"
)

var (
	ErrValidation = errors.New("admin report input failed validation")
	ErrNotFound   = errors.New("admin report record not found")
)

const (
	PaymentPending        = "pending_payment"
	PaymentPendingAlt     = "payment_pending"
	PaymentPaid           = "paid"
	PaymentFailed         = "payment_failed"
	PaymentCancelled      = "payment_cancelled"
	FulfillmentPending    = fulfillment.StatusPending
	FulfillmentNoStock    = fulfillment.LegacyStockUnavailable
	FulfillmentProcessing = fulfillment.StatusProcessing
	FulfillmentReady      = fulfillment.StatusReadyForDelivery
	FulfillmentOut        = fulfillment.StatusOutForDelivery
	FulfillmentDelivered  = fulfillment.StatusDelivered
	AttemptPending        = "pending"
	AttemptSucceeded      = "succeeded"
	AttemptFailed         = "failed"
	AttemptCancelled      = "cancelled"
	ProviderMock          = "mock"
	ProviderXendlyPay     = "xendlypay"
	VerificationVerified  = "verified"
	CurrencyRWF           = "RWF"
	DefaultRecentLimit    = 5
	RevenueAccountingRule = "Succeeded non-mock attempts with a verified payment event; each distinct order is counted once. Mock settlements are simulations and are never real revenue."
)

// OrderFilter is validated from the query string by the HTTP layer and again
// by the service. Only allowlisted values reach parameterized SQL.
type OrderFilter struct {
	Page              int
	Limit             int
	Search            string
	OrderReference    string
	OrderStatus       string
	PaymentStatus     string
	FulfillmentStatus string
	LocalDelivery     string
	FromDate          string
	ToDate            string
	Sort              string
}

type OrderSummary struct {
	ID                string    `json:"id"`
	Reference         string    `json:"reference"`
	CustomerID        string    `json:"customerId,omitempty"`
	OrderedAt         time.Time `json:"orderedAt"`
	OrderStatus       string    `json:"orderStatus"`
	PaymentStatus     string    `json:"paymentStatus"`
	FulfillmentStatus string    `json:"fulfillmentStatus"`
	CustomerName      string    `json:"customerName"`
	CustomerEmail     string    `json:"customerEmail"`
	CustomerPhone     string    `json:"customerPhone"`
	Currency          string    `json:"currency"`
	SubtotalRWF       int64     `json:"subtotalRwf"`
	TotalRWF          int64     `json:"totalRwf"`
	ItemCount         int       `json:"itemCount"`
	LatestAttempt     *Attempt  `json:"latestAttempt,omitempty"`
}

type OrderPage struct {
	Items   []OrderSummary `json:"items"`
	Page    int            `json:"page"`
	Limit   int            `json:"limit"`
	HasNext bool           `json:"hasNext"`
}

type Contact struct {
	FullName  string `json:"fullName"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	PhoneE164 string `json:"phoneE164"`
}

type Delivery struct {
	Country      string  `json:"country"`
	Province     string  `json:"province"`
	District     string  `json:"district"`
	Sector       string  `json:"sector"`
	Cell         *string `json:"cell"`
	AddressLine  string  `json:"addressLine"`
	Instructions *string `json:"instructions"`
}

type Item struct {
	ID           string  `json:"id"`
	ProductID    string  `json:"productId"`
	SKU          string  `json:"sku"`
	NameEN       string  `json:"nameEn"`
	NameRW       *string `json:"nameRw"`
	Condition    string  `json:"condition"`
	Quantity     int     `json:"quantity"`
	UnitPriceRWF int64   `json:"unitPriceRwf"`
	LineTotalRWF int64   `json:"lineTotalRwf"`
}

type Attempt struct {
	ID                string       `json:"id"`
	OrderID           string       `json:"orderId"`
	OrderReference    string       `json:"orderReference"`
	Provider          string       `json:"provider"`
	Environment       string       `json:"environment"`
	IsMock            bool         `json:"isMock"`
	State             string       `json:"state"`
	AmountRWF         int64        `json:"amountRwf"`
	Currency          string       `json:"currency"`
	Phone             string       `json:"phone"`
	PhoneE164         string       `json:"phoneE164"`
	Network           string       `json:"network"`
	PaymentMethod     string       `json:"paymentMethod"`
	ProviderReference *string      `json:"providerReference"`
	PromptStatus      string       `json:"promptStatus"`
	FailureCode       *string      `json:"failureCode"`
	FailureMessage    *string      `json:"failureMessage"`
	StockOutcome      string       `json:"stockOutcome"`
	CreatedAt         time.Time    `json:"createdAt"`
	UpdatedAt         time.Time    `json:"updatedAt"`
	CompletedAt       *time.Time   `json:"completedAt"`
	Transitions       []Transition `json:"transitions,omitempty"`
}

type Transition struct {
	FromState  *string   `json:"fromState"`
	ToState    string    `json:"toState"`
	EventType  string    `json:"eventType"`
	Actor      string    `json:"actor"`
	DeliveryID *string   `json:"deliveryId"`
	OccurredAt time.Time `json:"occurredAt"`
}

// AttemptSummary separates simulation totals from verified real revenue. Money
// fields use the distinct order total, never the sum of retry attempts.
type AttemptSummary struct {
	TotalAttempts         int64    `json:"totalAttempts"`
	PendingAttempts       int64    `json:"pendingAttempts"`
	SucceededAttempts     int64    `json:"succeededAttempts"`
	FailedAttempts        int64    `json:"failedAttempts"`
	CancelledAttempts     int64    `json:"cancelledAttempts"`
	MockSucceededAttempts int64    `json:"mockSucceededAttempts"`
	LiveSucceededAttempts int64    `json:"liveSucceededAttempts"`
	VerifiedLiveAttempts  int64    `json:"verifiedLiveAttempts"`
	MockSettledAmountRWF  int64    `json:"mockSettledAmountRwf"`
	RealVerifiedAmountRWF int64    `json:"realVerifiedAmountRwf"`
	MockRevenueIsReal     bool     `json:"mockRevenueIsReal"`
	LatestAttempt         *Attempt `json:"latestAttempt,omitempty"`
}

type Payment struct {
	Status         string         `json:"status"`
	AttemptSummary AttemptSummary `json:"attemptSummary"`
}

type Fulfillment struct {
	Status               string     `json:"status"`
	ReservationStatus    string     `json:"reservationStatus"`
	ReservationExpiresAt *time.Time `json:"reservationExpiresAt"`
	ReleasePolicy        string     `json:"releasePolicy"`
}

type OrderDetail struct {
	Order                 OrderSummary   `json:"order"`
	Contact               Contact        `json:"contact"`
	Delivery              Delivery       `json:"delivery"`
	Items                 []Item         `json:"items"`
	Payment               Payment        `json:"payment"`
	Fulfillment           Fulfillment    `json:"fulfillment"`
	PaymentAttempts       []Attempt      `json:"paymentAttempts"`
	PaymentAttemptSummary AttemptSummary `json:"paymentAttemptSummary"`
}

type AttemptPage struct {
	Items   []Attempt      `json:"items"`
	Page    int            `json:"page"`
	Limit   int            `json:"limit"`
	HasNext bool           `json:"hasNext"`
	Summary AttemptSummary `json:"summary"`
}

type RecentOrder struct {
	ID                string    `json:"id"`
	Reference         string    `json:"reference"`
	OrderedAt         time.Time `json:"orderedAt"`
	OrderStatus       string    `json:"orderStatus"`
	PaymentStatus     string    `json:"paymentStatus"`
	FulfillmentStatus string    `json:"fulfillmentStatus"`
	CustomerName      string    `json:"customerName"`
	CustomerPhone     string    `json:"customerPhone"`
	TotalRWF          int64     `json:"totalRwf"`
	ItemCount         int       `json:"itemCount"`
}

type Dashboard struct {
	GeneratedAt            time.Time        `json:"generatedAt"`
	TotalOrders            int64            `json:"totalOrders"`
	PendingPaymentOrders   int64            `json:"pendingPaymentOrders"`
	MockSuccessfulOrders   int64            `json:"mockSuccessfulOrders"`
	FailedPayments         int64            `json:"failedPayments"`
	FailedPaymentOrders    int64            `json:"failedPaymentOrders"`
	LiveVerifiedAttempts   int64            `json:"liveVerifiedAttempts"`
	RealVerifiedRevenueRWF int64            `json:"realVerifiedRevenueRwf"`
	RealVerifiedOrders     int64            `json:"realVerifiedOrders"`
	MockSettledAmountRWF   int64            `json:"mockSettledAmountRwf"`
	MockRevenueIsReal      bool             `json:"mockRevenueIsReal"`
	RevenuePolicy          string           `json:"revenuePolicy"`
	FulfillmentCounts      map[string]int64 `json:"fulfillmentCounts"`
	PaymentStatusCounts    map[string]int64 `json:"paymentStatusCounts"`
	RecentOrders           []RecentOrder    `json:"recentOrders"`
}
