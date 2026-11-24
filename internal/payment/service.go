package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"autivashop/backend/internal/config"
	"autivashop/backend/internal/product"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound            = errors.New("payment attempt not found")
	ErrValidation          = errors.New("payment input failed validation")
	ErrIdempotencyConflict = errors.New("payment idempotency key reused with a different phone")
	ErrConflict            = errors.New("payment request conflicts with current order state")
	ErrInvalidTransition   = errors.New("payment state transition is not allowed")
	ErrReservationExpired  = errors.New("order reservation expired before payment")
	ErrAlreadyPaid         = errors.New("order already has a paid payment")
	ErrStockUnavailable    = errors.New("stock unavailable for simulated payment")
)

// PromptMessage never claims that a real phone prompt was sent.
const PromptMessage = "Simulated prompt pending. No phone message or payment request was sent."

type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Service owns the provider-neutral business logic. It never makes external
// calls itself; provider execution is delegated to the injected Provider.
type Service struct {
	pool     *pgxpool.Pool
	provider Provider
	cfg      config.Config
	now      func() time.Time
}

func NewService(pool *pgxpool.Pool, provider Provider, cfg config.Config) (*Service, error) {
	if pool == nil || provider == nil {
		return nil, ErrProviderUnavailable
	}
	if provider.Name() != cfg.Payment.Provider || provider.IsMock() != cfg.Payment.IsMock {
		return nil, ErrProviderUnavailable
	}
	return &Service{pool: pool, provider: provider, cfg: cfg, now: time.Now}, nil
}

// SetNow is a test hook for reservation-expiry scenarios.
func (s *Service) SetNow(now func() time.Time) { s.now = now }

func (s *Service) Provider() Provider { return s.provider }

func isUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == constraint
}

// Attempt is the canonical customer-visible payment projection. Provider
// secrets, raw fingerprints, PIN data and provider credentials are absent.
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
	Prompt            PromptView   `json:"prompt"`
	FailureCode       *string      `json:"failureCode"`
	FailureMessage    *string      `json:"failureMessage"`
	StockOutcome      string       `json:"stockOutcome"`
	CreatedAt         time.Time    `json:"createdAt"`
	UpdatedAt         time.Time    `json:"updatedAt"`
	CompletedAt       *time.Time   `json:"completedAt"`
	Transitions       []Transition `json:"transitions,omitempty"`
}

// PromptView is honest about the local simulation.
type PromptView struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type Transition struct {
	FromState  *string   `json:"fromState"`
	ToState    string    `json:"toState"`
	EventType  string    `json:"eventType"`
	Actor      string    `json:"actor"`
	DeliveryID *string   `json:"deliveryId"`
	OccurredAt time.Time `json:"occurredAt"`
}

type AdminAttempt struct {
	Attempt
	CustomerName       string   `json:"customerName"`
	CustomerEmail      string   `json:"customerEmail"`
	CustomerPhone      string   `json:"customerPhone"`
	DeliveryLocation   Delivery `json:"deliveryLocation"`
	OrderPaymentStatus string   `json:"orderPaymentStatus"`
	FulfillmentStatus  string   `json:"fulfillmentStatus"`
}

type Delivery struct {
	Country     string  `json:"country"`
	Province    string  `json:"province"`
	District    string  `json:"district"`
	Sector      string  `json:"sector"`
	Cell        *string `json:"cell"`
	AddressLine string  `json:"addressLine"`
}

type Revenue struct {
	MockTotalRWF  int64 `json:"mockTotalRwf"`
	LiveTotalRWF  int64 `json:"liveTotalRwf"`
	MockPaidCount int64 `json:"mockPaidCount"`
	LivePaidCount int64 `json:"livePaidCount"`
	Separated     bool  `json:"separated"`
}

type Filter struct {
	Page           int
	Limit          int
	State          string
	Provider       string
	OrderReference string
	FromDate       string
	ToDate         string
}

var (
	datePattern           = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	orderReferencePattern = regexp.MustCompile(`^AV-[0-9]{8}-[A-Z0-9]{12}$`)
)

func promptView(status string) PromptView {
	message := PromptMessage
	if status == PromptSimulatedApproved {
		message = "Simulated approval recorded locally. No real payment was collected."
	}
	if status == PromptSimulatedRejected || status == PromptSimulatedTimeout {
		message = "Simulated failure recorded locally. No phone prompt was sent."
	}
	return PromptView{Status: status, Message: message}
}

func hashParts(parts ...string) []byte {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return sum[:]
}

func providerIdempotencyKey(orderID, key string) string {
	sum := sha256.Sum256([]byte("autivashop-provider|" + orderID + "|" + key))
	return "av-" + hex.EncodeToString(sum[:12])
}

func maskPhone(e164 string) string {
	if len(e164) < 10 {
		return "••••"
	}
	return e164[:6] + "••••" + e164[len(e164)-3:]
}

// displayPhone formats a validated Rwanda E.164 number for customer-facing
// projection. It is derived server-side from the normalized MSISDN.
func displayPhone(e164 string) string {
	if len(e164) != 13 || !strings.HasPrefix(e164, "+250") {
		return e164
	}
	return "+250 " + e164[4:7] + " " + e164[7:10] + " " + e164[10:]
}

func validDate(value string) bool {
	if value == "" {
		return true
	}
	if !datePattern.MatchString(value) {
		return false
	}
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

// transitionEvent maps the provider-style event recorded in payment_events to
// the simulation transition names allowed by the payment_transitions check.
func transitionEvent(eventType, failureCode string) string {
	switch eventType {
	case "payment.succeeded":
		return "simulation.approved"
	case "payment.failed":
		if failureCode == "SIMULATED_TIMEOUT" {
			return "simulation.timeout"
		}
		return "simulation.rejected"
	default:
		return eventType
	}
}

// Initiate validates ownership and authoritative order values, creates one
// pending attempt, and returns the provider's simulated pending result.
func (s *Service) Initiate(ctx context.Context, customerID, orderID, idempotencyKey, rawPhone string) (Attempt, bool, error) {
	if !product.ValidID(customerID) || !product.ValidID(orderID) || !ValidIdempotencyKey(idempotencyKey) {
		return Attempt{}, false, ErrValidation
	}
	if s.cfg.IsProduction() || !s.provider.IsMock() {
		return Attempt{}, false, ErrProviderUnavailable
	}
	msisdn, e164, network, err := NormalizeRwandaMSISDN(rawPhone)
	if err != nil {
		return Attempt{}, false, ErrValidation
	}
	fingerprint := hashParts(orderID, idempotencyKey, msisdn)
	providerKey := providerIdempotencyKey(orderID, idempotencyKey)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Attempt{}, false, fmt.Errorf("begin payment initiation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var orderRef, orderStatus, fulfillment string
	var amount int64
	var orderCustomer string
	err = tx.QueryRow(ctx, `
		SELECT o.reference, o.total_rwf, o.customer_id, o.payment_status, o.fulfillment_status
		FROM orders o WHERE o.id=$1 FOR UPDATE`, orderID).Scan(
		&orderRef, &amount, &orderCustomer, &orderStatus, &fulfillment)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && orderCustomer != customerID) {
		return Attempt{}, false, ErrNotFound
	}
	if err != nil {
		return Attempt{}, false, err
	}
	// A failed or cancelled attempt releases its reservation; the same order may
	// be retried with a new idempotency key. A paid order is rejected below by
	// the authoritative succeeded-attempt check.
	if orderStatus != "pending_payment" && orderStatus != "payment_pending" &&
		orderStatus != "payment_failed" && orderStatus != "payment_cancelled" {
		return Attempt{}, false, ErrConflict
	}
	if fulfillment != "pending" {
		return Attempt{}, false, ErrConflict
	}
	if amount < XendlyMinAmountRWF {
		return Attempt{}, false, ErrValidation
	}

	var paid bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM payment_attempts WHERE order_id=$1 AND state='succeeded')`, orderID).Scan(&paid); err != nil {
		return Attempt{}, false, err
	}
	if paid {
		return Attempt{}, false, ErrAlreadyPaid
	}
	var reserved bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM inventory_reservations
		 WHERE order_id=$1 AND status='active' AND expires_at > now())`, orderID).Scan(&reserved); err != nil {
		return Attempt{}, false, err
	}
	if !reserved {
		// A failed/cancelled attempt released its reservation. Retrying that
		// order re-checks stock and reactivates the same reservation row; it
		// never reuses stock that another order has since reserved.
		if orderStatus == "payment_failed" || orderStatus == "payment_cancelled" {
			renewed, renewErr := s.renewReservation(ctx, tx, orderID)
			if renewErr != nil {
				return Attempt{}, false, renewErr
			}
			if renewed {
				reserved = true
			}
		}
		if !reserved {
			return Attempt{}, false, ErrReservationExpired
		}
	}

	var existingID string
	var existingFingerprint []byte
	err = tx.QueryRow(ctx, `
		SELECT id, request_fingerprint FROM payment_attempts
		WHERE order_id=$1 AND idempotency_key=$2`, orderID, idempotencyKey).Scan(&existingID, &existingFingerprint)
	if err == nil {
		if !equalBytes(existingFingerprint, fingerprint) {
			return Attempt{}, false, ErrIdempotencyConflict
		}
		existing, readErr := readAttempt(ctx, tx, existingID)
		if readErr != nil {
			return Attempt{}, false, readErr
		}
		return existing, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, false, err
	}

	result, err := s.provider.Initiate(ctx, InitiateRequest{
		OrderID: orderID, OrderReference: orderRef, AmountRWF: amount, Currency: "RWF",
		MSISDN: msisdn, PhoneE164: e164, Network: network,
		IdempotencyKey: idempotencyKey, ProviderIdempotencyKey: providerKey,
		Description: "Order " + orderRef,
		Metadata:    map[string]string{"order_id": orderID, "order_reference": orderRef},
	})
	if err != nil {
		return Attempt{}, false, err
	}
	if result.State != StatePending || result.PromptStatus != PromptSimulatedPending {
		return Attempt{}, false, ErrProviderUnavailable
	}

	now := s.now().UTC()
	var attemptID string
	err = tx.QueryRow(ctx, `
		INSERT INTO payment_attempts (
			order_id, customer_id, order_reference, provider_name, provider_environment, is_mock,
			idempotency_key, provider_idempotency_key, request_fingerprint, provider_reference,
			amount_rwf, currency, customer_phone_e164, customer_msisdn, customer_phone_display, network, payment_method,
			state, prompt_status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'RWF',$12,$13,$14,$15,'prompt',$16,$17,$18,$18)
		RETURNING id`,
		orderID, customerID, orderRef, s.provider.Name(), s.provider.Environment(),
		s.provider.IsMock(), idempotencyKey, providerKey, fingerprint, result.ProviderReference,
		amount, e164, msisdn, displayPhone(e164), network, StatePending, PromptSimulatedPending, now).Scan(&attemptID)
	if err != nil {
		if isUniqueViolation(err, "payment_attempts_order_idempotency_key") ||
			isUniqueViolation(err, "payment_attempts_order_pending") ||
			isUniqueViolation(err, "payment_attempts_provider_key") ||
			isUniqueViolation(err, "payment_attempts_provider_reference") {
			return Attempt{}, false, ErrConflict
		}
		return Attempt{}, false, fmt.Errorf("insert payment attempt: %w", err)
	}

	deliveryID := "init-" + attemptID
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_events (attempt_id, delivery_id, event_type, provider_name,
			provider_environment, verification_status, payload, processed_at)
		VALUES ($1,$2,'payment.initiated',$3,$4,'not_applicable',
			jsonb_build_object('simulated',true,'amount_rwf',$5::bigint,'currency','RWF'),$6)`,
		attemptID, deliveryID, s.provider.Name(), s.provider.Environment(), amount, now); err != nil {
		return Attempt{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_transitions (attempt_id, from_state, to_state, event_type, actor, delivery_id, metadata, occurred_at)
		VALUES ($1,NULL,$2,'payment.initiated','customer',$3,jsonb_build_object('provider',$4::text),$5)`,
		attemptID, StatePending, deliveryID, s.provider.Name(), now); err != nil {
		return Attempt{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_audit_events (attempt_id, order_id, customer_id, action, metadata)
		VALUES ($1,$2,$3,'payment.initiated',jsonb_build_object('provider',$4::text,'amount_rwf',$5::bigint))`,
		attemptID, orderID, customerID, s.provider.Name(), amount); err != nil {
		return Attempt{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_audit_events (order_id, customer_id, action, metadata)
		VALUES ($1,$2,'order.payment.initiated',jsonb_build_object('attempt_id',$3::text,'provider',$4::text))`,
		orderID, customerID, attemptID, s.provider.Name()); err != nil {
		return Attempt{}, false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET payment_status='payment_pending', updated_at=now() WHERE id=$1`, orderID); err != nil {
		return Attempt{}, false, err
	}
	attempt, err := readAttempt(ctx, tx, attemptID)
	if err != nil {
		return Attempt{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Attempt{}, false, err
	}
	return attempt, false, nil
}

// Simulate applies a controlled local outcome. It is registered only when the
// mock provider is explicitly selected in development/test. It never contacts
// a network and never claims that a phone prompt was delivered.
func (s *Service) Simulate(ctx context.Context, customerID, attemptID, outcome string) (Attempt, error) {
	if !product.ValidID(customerID) || !product.ValidID(attemptID) {
		return Attempt{}, ErrValidation
	}
	if s.cfg.IsProduction() || !s.provider.IsMock() {
		return Attempt{}, ErrProviderUnavailable
	}
	simulator, ok := s.provider.(Simulator)
	if !ok {
		return Attempt{}, ErrProviderUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Attempt{}, fmt.Errorf("begin payment simulation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var lockedOrderID string
	err = tx.QueryRow(ctx, `
		SELECT o.id FROM orders o JOIN payment_attempts pa ON pa.order_id=o.id
		WHERE pa.id=$1 FOR UPDATE OF o`, attemptID).Scan(&lockedOrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}

	var owner, state, orderRef, promptStatus string
	var amount int64
	var orderID string
	err = tx.QueryRow(ctx, `
		SELECT pa.order_id, pa.customer_id, pa.state, pa.prompt_status, pa.amount_rwf, o.reference
		FROM payment_attempts pa JOIN orders o ON o.id=pa.order_id
		WHERE pa.id=$1 FOR UPDATE OF pa`, attemptID).Scan(&orderID, &owner, &state, &promptStatus, &amount, &orderRef)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (owner != customerID || orderID != lockedOrderID)) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}

	result, err := simulator.Simulate(ctx, SimulationRequest{
		AttemptID: attemptID, Outcome: outcome, OrderReference: orderRef, AmountRWF: amount,
	})
	if err != nil {
		return Attempt{}, ErrValidation
	}

	// Replaying the same delivery is idempotent even after the attempt reached a
	// final state. A different outcome has a different delivery ID and is
	// rejected by the pending-state check below.
	var existingID string
	err = tx.QueryRow(ctx, `SELECT attempt_id FROM payment_events WHERE delivery_id=$1`, result.DeliveryID).Scan(&existingID)
	if err == nil {
		attempt, readErr := readAttempt(ctx, tx, existingID)
		if readErr != nil {
			return Attempt{}, readErr
		}
		return attempt, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, err
	}
	if state != StatePending || promptStatus != PromptSimulatedPending {
		return Attempt{}, ErrInvalidTransition
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_events (attempt_id, delivery_id, event_type, provider_name,
			provider_environment, verification_status, payload, received_at, processed_at)
		VALUES ($1,$2,$3,$4,$5,'not_applicable',$6,$7,$7)`,
		attemptID, result.DeliveryID, result.EventType, s.provider.Name(), s.provider.Environment(),
		mustJSON(result.Payload), result.OccurredAt); err != nil {
		if isUniqueViolation(err, "payment_events_delivery_id_key") {
			attempt, readErr := readAttempt(ctx, tx, attemptID)
			if readErr != nil {
				return Attempt{}, readErr
			}
			return attempt, tx.Commit(ctx)
		}
		return Attempt{}, err
	}

	stockOutcome := "not_applied"
	fulfillment := "pending"
	orderPayment := "payment_pending"
	var failureCode, failureMessage any
	if result.FailureCode != "" {
		failureCode = result.FailureCode
		failureMessage = result.FailureMessage
	}
	if result.State == StateSucceeded {
		consumed, err := s.consumeReservation(ctx, tx, orderID)
		if err != nil {
			return Attempt{}, err
		}
		if consumed {
			stockOutcome = "reservation_consumed"
		} else {
			stockOutcome = "late_success_blocked"
			fulfillment = "payment_received_stock_unavailable"
		}
		orderPayment = "paid"
	} else {
		stockOutcome = "reservation_released"
		orderPayment = map[string]string{StateFailed: "payment_failed", StateCancelled: "payment_cancelled"}[result.State]
		if orderPayment == "" {
			return Attempt{}, ErrInvalidTransition
		}
		if _, err := tx.Exec(ctx, `
			UPDATE inventory_reservations SET status='released', released_at=now()
			WHERE order_id=$1 AND status='active'`, orderID); err != nil {
			return Attempt{}, err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE payment_attempts SET state=$2, prompt_status=$3, failure_code=$4, failure_message=$5,
			stock_outcome=$6, completed_at=$7, updated_at=now() WHERE id=$1 AND state='pending'`,
		attemptID, result.State, result.PromptStatus, failureCode, failureMessage, stockOutcome, result.OccurredAt); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_transitions (attempt_id, from_state, to_state, event_type, actor, delivery_id, metadata, occurred_at)
		VALUES ($1,$2,$3,$4,'mock_simulation',$5,$6,$7)`,
		attemptID, StatePending, result.State, transitionEvent(result.EventType, result.FailureCode), result.DeliveryID,
		mustJSON(result.Payload), result.OccurredAt); err != nil {
		return Attempt{}, err
	}
	// Fulfillment may already have advanced under an Admin lock. Never overwrite
	// an authoritative fulfillment state with a payment-derived legacy value.
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET payment_status=$2,
			fulfillment_status=CASE
				WHEN fulfillment_status IN ('pending', 'payment_received_stock_unavailable') THEN $3
				ELSE fulfillment_status END,
			updated_at=now() WHERE id=$1`,
		orderID, orderPayment, fulfillment); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_audit_events (attempt_id, order_id, customer_id, action, metadata)
		VALUES ($1,$2,$3,$4,$5)`,
		attemptID, orderID, customerID,
		map[bool]string{true: "payment.simulated.success", false: "payment.simulated.failure"}[result.State == StateSucceeded],
		mustJSON(map[string]any{"state": result.State, "deliveryId": result.DeliveryID,
			"stockOutcome": stockOutcome, "simulated": true})); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_audit_events (order_id, customer_id, action, metadata)
		VALUES ($1,$2,$3,$4)`,
		orderID, customerID,
		map[bool]string{true: "order.payment.mock_approved", false: "order.payment.mock_failed"}[result.State == StateSucceeded],
		mustJSON(map[string]any{"attemptId": attemptID, "paymentStatus": orderPayment,
			"fulfillmentStatus": fulfillment, "stockOutcome": stockOutcome})); err != nil {
		return Attempt{}, err
	}

	attempt, err := readAttempt(ctx, tx, attemptID)
	if err != nil {
		return Attempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

// consumeReservation deducts physical stock exactly once for a successful
// payment. Product/inventory rows are locked in product-ID order; the
// reservation status comparison prevents duplicate deductions.
func (s *Service) consumeReservation(ctx context.Context, tx pgx.Tx, orderID string) (bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT oi.product_id, oi.quantity, ir.id, ir.status, ir.expires_at
		FROM order_items oi
		JOIN inventory_reservations ir ON ir.order_id=oi.order_id AND ir.product_id=oi.product_id
		WHERE oi.order_id=$1 ORDER BY oi.product_id`, orderID)
	if err != nil {
		return false, err
	}
	type item struct {
		productID     string
		quantity      int64
		reservationID string
		status        string
		expiresAt     time.Time
	}
	items := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.productID, &it.quantity, &it.reservationID, &it.status, &it.expiresAt); err != nil {
			rows.Close()
			return false, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if len(items) == 0 {
		return false, nil
	}
	for _, it := range items {
		if it.status != "active" || !it.expiresAt.After(s.now()) {
			return false, nil
		}
		var stock int64
		if err := tx.QueryRow(ctx,
			`SELECT stock_quantity FROM inventory WHERE product_id=$1 FOR UPDATE`, it.productID).Scan(&stock); err != nil {
			return false, err
		}
		if stock < it.quantity {
			return false, nil
		}
	}
	for _, it := range items {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory SET stock_quantity=stock_quantity-$2,
				availability=CASE WHEN stock_quantity-$2 = 0 THEN 'out-of-stock' ELSE availability END,
				updated_at=now()
			WHERE product_id=$1 AND stock_quantity >= $2`, it.productID, it.quantity)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() != 1 {
			// A partial deduction must never be preserved: return an error so the
			// caller rolls back the entire payment transaction.
			return false, fmt.Errorf("inventory deduction lost a locked row")
		}
		tag, err = tx.Exec(ctx, `
			UPDATE inventory_reservations SET status='consumed'
			WHERE id=$1 AND status='active' AND expires_at > now()`, it.reservationID)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() != 1 {
			return false, fmt.Errorf("reservation consumption lost a locked row")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_consumption_events
				(order_id, product_id, reservation_id, quantity, source)
			VALUES ($1,$2,$3,$4,'payment_simulation')`,
			orderID, it.productID, it.reservationID, it.quantity); err != nil {
			if isUniqueViolation(err, "inventory_consumption_events_pkey") {
				return false, fmt.Errorf("duplicate inventory consumption record")
			}
			return false, err
		}
	}
	return true, nil
}

// renewReservation re-checks current availability before reactivating the
// reservation released by a failed/cancelled attempt. Product and inventory
// rows are locked in product-ID order, and competing active reservations are
// excluded so retrying an old order cannot oversell stock.
func (s *Service) renewReservation(ctx context.Context, tx pgx.Tx, orderID string) (bool, error) {
	idRows, err := tx.Query(ctx,
		`SELECT product_id FROM order_items WHERE order_id=$1 ORDER BY product_id`, orderID)
	if err != nil {
		return false, err
	}
	productIDs := []string{}
	for idRows.Next() {
		var productID string
		if err := idRows.Scan(&productID); err != nil {
			idRows.Close()
			return false, err
		}
		productIDs = append(productIDs, productID)
	}
	if err := idRows.Err(); err != nil {
		idRows.Close()
		return false, err
	}
	idRows.Close()
	if len(productIDs) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(productIDs))
	lockArgs := make([]any, len(productIDs))
	for i, productID := range productIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		lockArgs[i] = productID
	}
	if _, err := tx.Exec(ctx,
		`SELECT p.id FROM products p JOIN inventory i ON i.product_id=p.id
		 WHERE p.id IN (`+strings.Join(placeholders, ",")+`) ORDER BY p.id FOR UPDATE OF p, i`,
		lockArgs...); err != nil {
		return false, err
	}

	rows, err := tx.Query(ctx, `
		SELECT oi.product_id, oi.quantity, ir.id, i.stock_quantity,
		       COALESCE((SELECT SUM(r.quantity) FROM inventory_reservations r
		                 WHERE r.product_id=oi.product_id AND r.status='active'
		                   AND r.expires_at > now() AND r.order_id <> oi.order_id), 0),
		       p.published, p.deleted_at IS NULL, i.availability
		FROM order_items oi
		JOIN inventory_reservations ir ON ir.order_id=oi.order_id AND ir.product_id=oi.product_id
		JOIN inventory i ON i.product_id=oi.product_id
		JOIN products p ON p.id=oi.product_id
		WHERE oi.order_id=$1 ORDER BY oi.product_id`, orderID)
	if err != nil {
		return false, err
	}
	type renewal struct {
		reservationID string
		quantity      int64
	}
	renewals := []renewal{}
	for rows.Next() {
		var productID string
		var quantity, stock, reserved int64
		var published, notDeleted bool
		var availability string
		var reservationID string
		if err := rows.Scan(&productID, &quantity, &reservationID, &stock, &reserved,
			&published, &notDeleted, &availability); err != nil {
			rows.Close()
			return false, err
		}
		if !published || !notDeleted || availability != "available" || stock-reserved < quantity {
			rows.Close()
			return false, ErrStockUnavailable
		}
		renewals = append(renewals, renewal{reservationID: reservationID, quantity: quantity})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if len(renewals) == 0 {
		return false, nil
	}
	seconds := int64(s.cfg.Orders.ReservationTTL / time.Second)
	if seconds < 300 {
		seconds = 1800
	}
	for _, item := range renewals {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_reservations SET status='active', created_at=now(),
				expires_at=now()+($2::bigint * interval '1 second')
			WHERE id=$1 AND status IN ('released','expired')`, item.reservationID, seconds)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() != 1 {
			return false, ErrStockUnavailable
		}
	}
	return true, nil
}

const attemptColumns = `
	pa.id, pa.order_id, pa.order_reference, pa.provider_name, pa.provider_environment, pa.is_mock,
	pa.state, pa.amount_rwf, pa.currency, pa.customer_phone_display, pa.customer_phone_e164,
	pa.network, pa.payment_method, pa.provider_reference, pa.prompt_status, pa.failure_code,
	pa.failure_message, pa.stock_outcome, pa.created_at, pa.updated_at, pa.completed_at`

const adminAttemptColumns = attemptColumns + `,
	o.customer_name, o.customer_email, o.customer_phone_e164,
	o.country, o.province, o.district, o.sector, o.cell, o.address_line,
	o.payment_status, o.fulfillment_status`

func scanAdminAttempt(rows pgx.Rows) (AdminAttempt, error) {
	var a AdminAttempt
	var phoneDisplay string
	if err := rows.Scan(&a.ID, &a.OrderID, &a.OrderReference, &a.Provider, &a.Environment, &a.IsMock,
		&a.State, &a.AmountRWF, &a.Currency, &phoneDisplay, &a.PhoneE164, &a.Network,
		&a.PaymentMethod, &a.ProviderReference, &a.Prompt.Status, &a.FailureCode,
		&a.FailureMessage, &a.StockOutcome, &a.CreatedAt, &a.UpdatedAt, &a.CompletedAt,
		&a.CustomerName, &a.CustomerEmail, &a.CustomerPhone,
		&a.DeliveryLocation.Country, &a.DeliveryLocation.Province, &a.DeliveryLocation.District,
		&a.DeliveryLocation.Sector, &a.DeliveryLocation.Cell, &a.DeliveryLocation.AddressLine,
		&a.OrderPaymentStatus, &a.FulfillmentStatus); err != nil {
		return AdminAttempt{}, err
	}
	a.Phone = maskPhone(a.PhoneE164)
	a.PhoneE164 = maskPhone(a.PhoneE164)
	a.CustomerPhone = maskPhone(a.CustomerPhone)
	a.Prompt.Message = promptView(a.Prompt.Status).Message
	return a, nil
}

func scanAttempt(rows pgx.Rows) (Attempt, error) {
	var a Attempt
	var phoneDisplay string
	err := rows.Scan(&a.ID, &a.OrderID, &a.OrderReference, &a.Provider, &a.Environment, &a.IsMock,
		&a.State, &a.AmountRWF, &a.Currency, &phoneDisplay, &a.PhoneE164, &a.Network,
		&a.PaymentMethod, &a.ProviderReference, &a.Prompt.Status, &a.FailureCode,
		&a.FailureMessage, &a.StockOutcome, &a.CreatedAt, &a.UpdatedAt, &a.CompletedAt)
	if err != nil {
		return Attempt{}, err
	}
	a.Phone = phoneDisplay
	a.Prompt.Message = promptView(a.Prompt.Status).Message
	return a, nil
}

func readAttempt(ctx context.Context, q queryer, id string) (Attempt, error) {
	rows, err := q.Query(ctx, `SELECT `+attemptColumns+` FROM payment_attempts pa WHERE pa.id=$1`, id)
	if err != nil {
		return Attempt{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Attempt{}, err
		}
		return Attempt{}, ErrNotFound
	}
	a, err := scanAttempt(rows)
	if err != nil {
		return Attempt{}, err
	}
	rows.Close()
	transitions, err := readTransitions(ctx, q, id)
	a.Transitions = transitions
	return a, err
}

func readTransitions(ctx context.Context, q queryer, attemptID string) ([]Transition, error) {
	rows, err := q.Query(ctx, `
		SELECT from_state, to_state, event_type, actor, delivery_id, occurred_at
		FROM payment_transitions WHERE attempt_id=$1 ORDER BY occurred_at, id`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Transition{}
	for rows.Next() {
		var tr Transition
		if err := rows.Scan(&tr.FromState, &tr.ToState, &tr.EventType, &tr.Actor, &tr.DeliveryID, &tr.OccurredAt); err != nil {
			return nil, err
		}
		items = append(items, tr)
	}
	return items, rows.Err()
}

// Get returns an attempt only to its owning customer and only under the order
// named in the request path.
func (s *Service) Get(ctx context.Context, customerID, orderID, attemptID string) (Attempt, error) {
	if !product.ValidID(customerID) || !product.ValidID(orderID) || !product.ValidID(attemptID) {
		return Attempt{}, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+attemptColumns+`
		FROM payment_attempts pa WHERE pa.id=$1 AND pa.customer_id=$2 AND pa.order_id=$3`,
		attemptID, customerID, orderID)
	if err != nil {
		return Attempt{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Attempt{}, err
		}
		return Attempt{}, ErrNotFound
	}
	a, err := scanAttempt(rows)
	if err != nil {
		return Attempt{}, err
	}
	rows.Close()
	transitions, err := readTransitions(ctx, s.pool, attemptID)
	a.Transitions = transitions
	return a, err
}

// ListForOrder returns the authenticated customer's attempts for one order.
func (s *Service) ListForOrder(ctx context.Context, customerID, orderID string) ([]Attempt, error) {
	if !product.ValidID(customerID) || !product.ValidID(orderID) {
		return nil, ErrValidation
	}
	rows, err := s.pool.Query(ctx, `SELECT `+attemptColumns+`
		FROM payment_attempts pa WHERE pa.order_id=$1 AND pa.customer_id=$2
		ORDER BY pa.created_at DESC, pa.id DESC`, orderID, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Attempt{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

// AdminGet returns one protected payment record. It is used only by the
// authenticated admin surface and masks every phone number in the projection.
func (s *Service) AdminGet(ctx context.Context, attemptID string) (AdminAttempt, error) {
	if !product.ValidID(attemptID) {
		return AdminAttempt{}, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+adminAttemptColumns+`
		FROM payment_attempts pa JOIN orders o ON o.id=pa.order_id WHERE pa.id=$1`, attemptID)
	if err != nil {
		return AdminAttempt{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return AdminAttempt{}, err
		}
		return AdminAttempt{}, ErrNotFound
	}
	a, err := scanAdminAttempt(rows)
	if err != nil {
		return AdminAttempt{}, err
	}
	rows.Close()
	transitions, err := readTransitions(ctx, s.pool, attemptID)
	a.Transitions = transitions
	return a, err
}

// AdminList returns protected payment records with masked phone numbers and
// delivery addresses copied from the immutable order snapshot.
func (s *Service) AdminList(ctx context.Context, f Filter) ([]AdminAttempt, bool, error) {
	if f.Page < 1 || f.Page > 1000 || f.Limit < 1 || f.Limit > 100 ||
		!validDate(f.FromDate) || !validDate(f.ToDate) {
		return nil, false, ErrValidation
	}
	if f.State != "" && f.State != StatePending && f.State != StateSucceeded &&
		f.State != StateFailed && f.State != StateCancelled {
		return nil, false, ErrValidation
	}
	if f.Provider != "" && f.Provider != "mock" && f.Provider != "xendlypay" {
		return nil, false, ErrValidation
	}
	if f.OrderReference != "" && !orderReferencePattern.MatchString(f.OrderReference) {
		return nil, false, ErrValidation
	}
	where := []string{"1=1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.State != "" {
		add("pa.state = $%d", f.State)
	}
	if f.Provider != "" {
		add("pa.provider_name = $%d", f.Provider)
	}
	if f.OrderReference != "" {
		add("pa.order_reference = $%d", f.OrderReference)
	}
	if f.FromDate != "" {
		add("pa.created_at >= $%d::date", f.FromDate)
	}
	if f.ToDate != "" {
		add("pa.created_at < $%d::date + interval '1 day'", f.ToDate)
	}
	args = append(args, f.Limit+1, (f.Page-1)*f.Limit)
	query := `SELECT ` + adminAttemptColumns + `
		FROM payment_attempts pa JOIN orders o ON o.id=pa.order_id
		WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY pa.created_at DESC, pa.id DESC LIMIT $` + fmt.Sprint(len(args)-1) +
		` OFFSET $` + fmt.Sprint(len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []AdminAttempt{}
	for rows.Next() {
		a, err := scanAdminAttempt(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasNext := len(items) > f.Limit
	if hasNext {
		items = items[:f.Limit]
	}
	return items, hasNext, nil
}

// Revenue totals only authoritative succeeded attempts and never counts an
// initiation or a pending/simulated-prompt state as money received.
func (s *Service) Revenue(ctx context.Context) (Revenue, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT is_mock, COALESCE(SUM(amount_rwf),0), count(*)
		FROM payment_attempts WHERE state='succeeded' GROUP BY is_mock`)
	if err != nil {
		return Revenue{}, err
	}
	defer rows.Close()
	summary := Revenue{Separated: true}
	for rows.Next() {
		var mock bool
		var total, count int64
		if err := rows.Scan(&mock, &total, &count); err != nil {
			return Revenue{}, err
		}
		if mock {
			summary.MockTotalRWF, summary.MockPaidCount = total, count
		} else {
			summary.LiveTotalRWF, summary.LivePaidCount = total, count
		}
	}
	return summary, rows.Err()
}

// ValidIdempotencyKey mirrors the order API's strict header policy.
func ValidIdempotencyKey(value string) bool {
	return len(value) >= 8 && len(value) <= 128 && regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(value)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func mustJSON(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return b
}
