package fulfillment

import (
	"context"
	"crypto/sha256"
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)

type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type scanner interface {
	Scan(dest ...any) error
}

// Service is the single writer of fulfillment state. All mutations happen in
// one row-locked transaction that also writes the audit transition.
type Service struct {
	pool *pgxpool.Pool
	cfg  config.Config
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg config.Config) *Service {
	return &Service{pool: pool, cfg: cfg, now: time.Now}
}

// SetNow is a test hook for expiry-related scenarios.
func (s *Service) SetNow(now func() time.Time) { s.now = now }

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func formatUUID(value pgtype.UUID) string {
	if !value.Valid {
		return ""
	}
	b := value.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func uuidString(value pgtype.UUID) any {
	if !value.Valid {
		return nil
	}
	return formatUUID(value)
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func fingerprint(cmd Command, note string) []byte {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"fulfillment-v1", cmd.OrderID, cmd.Event, fmt.Sprint(cmd.ExpectedVersion), note, cmd.IdempotencyKey,
	}, "|")))
	return sum[:]
}

func validateCommand(cmd Command) (string, error) {
	if !product.ValidID(cmd.OrderID) || !idempotencyKeyPattern.MatchString(cmd.IdempotencyKey) ||
		cmd.ExpectedVersion < 0 {
		return "", ErrValidation
	}
	if _, ok := transitions[cmd.Event]; !ok {
		return "", ErrValidation
	}
	note := strings.TrimSpace(cmd.Note)
	if len(note) > 500 {
		return "", ErrValidation
	}
	return note, nil
}

func validateActor(actor Actor) error {
	if !product.ValidID(actor.AdminID) || !product.ValidID(actor.SessionID) ||
		len(strings.TrimSpace(actor.Email)) < 3 || len(strings.TrimSpace(actor.Email)) > 320 {
		return ErrForbidden
	}
	if actor.Role != "admin" && actor.Role != "super_admin" {
		return ErrForbidden
	}
	return nil
}

const stateColumns = `
	f.order_id, o.reference, f.status, f.version, f.settlement_mode,
	f.delivery_country, f.delivery_province, f.delivery_district, f.delivery_sector,
	f.delivery_cell, f.delivery_address_line, f.delivery_instructions,
	f.contact_name, f.contact_phone_masked,
	f.delivered_at, f.confirmation_method, f.confirmation_source, f.confirmation_note,
	f.confirmed_by_admin_id, f.confirmed_by_session_id,
	f.updated_at, o.payment_status, o.fulfillment_status, o.customer_id`

type stateRecord struct {
	State         State
	PaymentStatus string
	OrderStatus   string
	CustomerID    string
	CustomerUUID  pgtype.UUID
}

func scanState(s scanner) (stateRecord, error) {
	var r stateRecord
	var cell, instructions, method, source, note pgtype.Text
	var deliveredAt pgtype.Timestamptz
	var customerID pgtype.UUID
	var confirmedByAdmin, confirmedBySession pgtype.UUID
	if err := s.Scan(
		&r.State.OrderID, &r.State.OrderReference, &r.State.Status, &r.State.Version,
		&r.State.SettlementMode, &r.State.LocalDelivery.Country, &r.State.LocalDelivery.Province,
		&r.State.LocalDelivery.District, &r.State.LocalDelivery.Sector, &cell,
		&r.State.LocalDelivery.AddressLine, &instructions, &r.State.LocalDelivery.ContactName,
		&r.State.LocalDelivery.PhoneMasked, &deliveredAt, &method, &source, &note,
		&confirmedByAdmin, &confirmedBySession,
		&r.State.UpdatedAt, &r.PaymentStatus, &r.OrderStatus, &customerID); err != nil {
		return stateRecord{}, err
	}
	if cell.Valid {
		value := cell.String
		r.State.LocalDelivery.Cell = &value
	}
	if instructions.Valid {
		value := instructions.String
		r.State.LocalDelivery.Instructions = &value
	}
	if deliveredAt.Valid {
		confirmation := &Confirmation{
			ConfirmedAt: deliveredAt.Time,
			Method:      method.String,
			Source:      source.String,
			ConfirmedBy: "administrator",
		}
		if note.Valid {
			value := note.String
			confirmation.Note = &value
		}
		r.State.Confirmation = confirmation
	}
	r.State.CanTransitionTo = append([]string(nil), nextEvents[r.State.Status]...)
	r.CustomerUUID = customerID
	r.CustomerID = formatUUID(customerID)
	return r, nil
}

func (s *Service) readState(ctx context.Context, q queryer, orderID string) (stateRecord, error) {
	record, err := scanState(q.QueryRow(ctx, `SELECT `+stateColumns+`
		FROM order_fulfillment f JOIN orders o ON o.id=f.order_id WHERE f.order_id=$1`, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return stateRecord{}, ErrNotFound
	}
	return record, err
}

// GetState exposes the current authoritative fulfillment aggregate.
func (s *Service) GetState(ctx context.Context, orderID string) (State, error) {
	if !product.ValidID(orderID) {
		return State{}, ErrValidation
	}
	record, err := s.readState(ctx, s.pool, orderID)
	if err != nil {
		return State{}, err
	}
	return record.State, nil
}

// GetHistory returns the immutable Admin audit trail, newest first.
func (s *Service) GetHistory(ctx context.Context, orderID string) ([]AdminTransition, error) {
	if !product.ValidID(orderID) {
		return nil, ErrValidation
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, from_status, to_status, version, event_type, actor_type,
		       actor_admin_id, actor_email, actor_role, note, idempotency_key, occurred_at
		FROM fulfillment_transitions WHERE order_id=$1 ORDER BY version DESC, id DESC`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := []AdminTransition{}
	for rows.Next() {
		var item AdminTransition
		var adminID, email, role, note pgtype.Text
		if err := rows.Scan(&item.ID, &item.FromStatus, &item.ToStatus, &item.Version, &item.EventType,
			&item.ActorType, &adminID, &email, &role, &note, &item.IdempotencyKey, &item.OccurredAt); err != nil {
			return nil, err
		}
		if adminID.Valid {
			value := adminID.String
			item.ActorAdminID = &value
		}
		if email.Valid {
			value := email.String
			item.ActorEmail = &value
		}
		if role.Valid {
			value := role.String
			item.ActorRole = &value
		}
		if note.Valid {
			value := note.String
			item.Note = &value
		}
		history = append(history, item)
	}
	return history, rows.Err()
}

// CustomerGet is owner-scoped and returns no admin identity or internal note.
func (s *Service) CustomerGet(ctx context.Context, customerID, orderID string) (CustomerView, error) {
	if !product.ValidID(customerID) || !product.ValidID(orderID) {
		return CustomerView{}, ErrValidation
	}
	record, err := s.readState(ctx, s.pool, orderID)
	if err != nil {
		return CustomerView{}, err
	}
	if record.CustomerID != customerID {
		return CustomerView{}, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT from_status, to_status, event_type, occurred_at
		FROM fulfillment_transitions WHERE order_id=$1 ORDER BY version ASC, id ASC`, orderID)
	if err != nil {
		return CustomerView{}, err
	}
	defer rows.Close()
	history := []CustomerTransition{}
	for rows.Next() {
		var item CustomerTransition
		if err := rows.Scan(&item.FromStatus, &item.ToStatus, &item.EventType, &item.OccurredAt); err != nil {
			return CustomerView{}, err
		}
		history = append(history, item)
	}
	if err := rows.Err(); err != nil {
		return CustomerView{}, err
	}
	return CustomerView{
		State:         record.State,
		History:       history,
		PaymentStatus: record.PaymentStatus,
		OrderStatus:   record.OrderStatus,
	}, nil
}

// GetConflicts computes current blockers without changing any data.
func (s *Service) GetConflicts(ctx context.Context, orderID string) ([]Conflict, error) {
	if !product.ValidID(orderID) {
		return nil, ErrValidation
	}
	record, err := s.readState(ctx, s.pool, orderID)
	if err != nil {
		return nil, err
	}
	conflicts := []Conflict{}
	if record.PaymentStatus != "paid" {
		conflicts = append(conflicts, Conflict{Code: "payment.not_paid",
			Message: "The order has no authoritative paid settlement."})
	}
	var succeeded, mock, verified bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM payment_attempts WHERE order_id=$1 AND state='succeeded'),
		       COALESCE((SELECT is_mock FROM payment_attempts
		                 WHERE order_id=$1 AND state='succeeded'
		                 ORDER BY completed_at DESC NULLS LAST, id DESC LIMIT 1), false),
		       EXISTS(SELECT 1 FROM payment_events pe
		              JOIN payment_attempts pa ON pa.id=pe.attempt_id
		              WHERE pa.order_id=$1 AND pa.state='succeeded' AND pe.verification_status='verified')`,
		orderID).Scan(&succeeded, &mock, &verified); err != nil {
		return nil, err
	}
	if !succeeded {
		conflicts = append(conflicts, Conflict{Code: "payment.no_succeeded_attempt",
			Message: "No successful payment attempt is recorded for this order."})
	}
	if mock && (!s.cfg.Fulfillment.AllowSimulated || s.cfg.IsProduction()) {
		conflicts = append(conflicts, Conflict{Code: "payment.simulated_not_allowed",
			Message: "This settlement is a simulation and cannot authorize fulfillment in this environment."})
	}
	if !mock && succeeded && !verified {
		conflicts = append(conflicts, Conflict{Code: "payment.not_verified",
			Message: "The successful attempt has no verified payment event."})
	}
	if record.OrderStatus == LegacyStockUnavailable {
		conflicts = append(conflicts, Conflict{Code: "inventory.stock_unavailable",
			Message: "Stock was unavailable at settlement; this order requires manual resolution."})
	}
	allocations, err := s.allocationRows(ctx, s.pool, orderID)
	if err != nil {
		return nil, err
	}
	for _, item := range allocations {
		if !item.resolved {
			conflicts = append(conflicts, Conflict{Code: "inventory.allocation_unresolved",
				Message: "One or more items have no consumed inventory allocation for this order."})
			break
		}
	}
	delivery := record.State.LocalDelivery
	if delivery.Country != CountryRW || strings.TrimSpace(delivery.Province) == "" ||
		strings.TrimSpace(delivery.District) == "" || strings.TrimSpace(delivery.Sector) == "" ||
		strings.TrimSpace(delivery.AddressLine) == "" {
		conflicts = append(conflicts, Conflict{Code: "delivery.snapshot_invalid",
			Message: "The order has no complete Rwanda local-delivery snapshot."})
	}
	return conflicts, nil
}

type allocation struct {
	productID string
	status    pgtype.Text
	quantity  pgtype.Int8
	resolved  bool
}

func (s *Service) allocationRows(ctx context.Context, q queryer, orderID string) ([]allocation, error) {
	rows, err := q.Query(ctx, `
		SELECT oi.product_id, r.status, ice.quantity
		FROM order_items oi
		LEFT JOIN inventory_reservations r ON r.order_id=oi.order_id AND r.product_id=oi.product_id
		LEFT JOIN inventory_consumption_events ice ON ice.order_id=oi.order_id AND ice.product_id=oi.product_id
		WHERE oi.order_id=$1 ORDER BY oi.product_id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []allocation{}
	for rows.Next() {
		var item allocation
		var status pgtype.Text
		var quantity pgtype.Int8
		if err := rows.Scan(&item.productID, &status, &quantity); err != nil {
			return nil, err
		}
		item.status = status
		item.quantity = quantity
		item.resolved = status.Valid && status.String == "consumed" && quantity.Valid && quantity.Int64 > 0
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrAllocationUnresolved
	}
	return items, nil
}

func (s *Service) eligibility(ctx context.Context, tx pgx.Tx, orderID, settlementMode, orderPayment string) (string, error) {
	if orderPayment != "paid" {
		return "", ErrPaymentNotEligible
	}
	var attemptID string
	var isMock bool
	var verified bool
	err := tx.QueryRow(ctx, `
		SELECT pa.id, pa.is_mock,
		       EXISTS(SELECT 1 FROM payment_events pe
		              WHERE pe.attempt_id=pa.id AND pe.verification_status='verified')
		FROM payment_attempts pa
		WHERE pa.order_id=$1 AND pa.state='succeeded'
		ORDER BY pa.completed_at DESC NULLS LAST, pa.id DESC LIMIT 1`, orderID).Scan(&attemptID, &isMock, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPaymentNotEligible
	}
	if err != nil {
		return "", err
	}
	_ = attemptID
	var determined string
	switch {
	case isMock:
		determined = SettlementSimulated
	case verified:
		determined = SettlementLiveVerified
	default:
		return "", ErrPaymentNotEligible
	}
	if determined == SettlementSimulated && (!s.cfg.Fulfillment.AllowSimulated || s.cfg.IsProduction()) {
		return "", ErrForbidden
	}
	if settlementMode != SettlementNotPaid && settlementMode != determined {
		return "", ErrPaymentNotEligible
	}
	return determined, nil
}

// Transition is the only mutation entry point. It validates the command,
// serializes on the order row, checks backend-authoritative payment and
// allocation state, writes the audit row, then advances both projections.
func (s *Service) Transition(ctx context.Context, actor Actor, cmd Command) (TransitionResult, error) {
	note, err := validateCommand(cmd)
	if err != nil {
		return TransitionResult{}, err
	}
	if err := validateActor(actor); err != nil {
		return TransitionResult{}, err
	}
	transition := transitions[cmd.Event]
	fingerprint := fingerprint(cmd, note)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("begin fulfillment transition: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var current stateRecord
	current, err = scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+`
		FROM order_fulfillment f JOIN orders o ON o.id=f.order_id
		WHERE f.order_id=$1 FOR UPDATE OF f, o`, cmd.OrderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TransitionResult{}, ErrNotFound
	}
	if err != nil {
		return TransitionResult{}, err
	}

	var existingFingerprint []byte
	err = tx.QueryRow(ctx, `
		SELECT request_fingerprint FROM fulfillment_transitions
		WHERE order_id=$1 AND idempotency_key=$2`, cmd.OrderID, cmd.IdempotencyKey).Scan(&existingFingerprint)
	if err == nil {
		if !equalBytes(existingFingerprint, fingerprint) {
			return TransitionResult{}, ErrIdempotencyConflict
		}
		replayed, readErr := s.readState(ctx, tx, cmd.OrderID)
		if readErr != nil {
			return TransitionResult{}, readErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return TransitionResult{}, commitErr
		}
		return TransitionResult{State: replayed.State, Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TransitionResult{}, err
	}

	if cmd.ExpectedVersion != current.State.Version {
		return TransitionResult{}, ErrStaleVersion
	}
	if current.State.Status != transition.from {
		return TransitionResult{}, ErrInvalidTransition
	}
	if current.OrderStatus == LegacyStockUnavailable {
		return TransitionResult{}, ErrStockUnavailable
	}
	if current.State.Status == StatusDelivered {
		return TransitionResult{}, ErrInvalidTransition
	}
	delivery := current.State.LocalDelivery
	if delivery.Country != CountryRW || strings.TrimSpace(delivery.Province) == "" ||
		strings.TrimSpace(delivery.District) == "" || strings.TrimSpace(delivery.Sector) == "" ||
		strings.TrimSpace(delivery.AddressLine) == "" {
		return TransitionResult{}, ErrValidation
	}

	settlement := current.State.SettlementMode
	if settlement == SettlementNotPaid {
		settlement, err = s.eligibility(ctx, tx, cmd.OrderID, settlement, current.PaymentStatus)
		if err != nil {
			return TransitionResult{}, err
		}
	}

	allocations, err := s.allocationRows(ctx, tx, cmd.OrderID)
	if err != nil {
		return TransitionResult{}, err
	}
	for _, item := range allocations {
		if !item.resolved {
			if item.status.Valid && (item.status.String == "expired" || item.status.String == "released") {
				return TransitionResult{}, ErrStockUnavailable
			}
			return TransitionResult{}, ErrAllocationUnresolved
		}
	}

	metadata := mustJSON(map[string]any{
		"settlementMode":    settlement,
		"simulated":         settlement == SettlementSimulated,
		"orderPayment":      current.PaymentStatus,
		"actorRole":         actor.Role,
		"deliveryConfirmed": transition.to == StatusDelivered,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO fulfillment_transitions (
			order_id, from_status, to_status, version, event_type, actor_type,
			actor_admin_id, actor_session_id, actor_email, actor_role, note,
			idempotency_key, request_fingerprint, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb)`,
		cmd.OrderID, transition.from, transition.to, cmd.ExpectedVersion+1, transition.event,
		ActorAdmin, actor.AdminID, actor.SessionID, strings.TrimSpace(actor.Email), actor.Role,
		nullableString(note), cmd.IdempotencyKey, fingerprint, metadata); err != nil {
		return TransitionResult{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE order_fulfillment SET
			status=$2, version=$3, settlement_mode=$4,
			delivered_at=CASE WHEN $2='delivered' THEN now() ELSE NULL END,
			confirmation_method=CASE WHEN $2='delivered' THEN $5 ELSE NULL END,
			confirmation_source=CASE WHEN $2='delivered' THEN $6 ELSE NULL END,
			confirmation_note=CASE WHEN $2='delivered' THEN $7 ELSE NULL END,
			confirmed_by_admin_id=CASE WHEN $2='delivered' THEN $8::uuid ELSE NULL END,
			confirmed_by_session_id=CASE WHEN $2='delivered' THEN $9::uuid ELSE NULL END
		WHERE order_id=$1`,
		cmd.OrderID, transition.to, cmd.ExpectedVersion+1, settlement,
		map[bool]string{true: ConfirmationMethod, false: ""}[transition.to == StatusDelivered],
		map[bool]string{true: ConfirmationSource, false: ""}[transition.to == StatusDelivered],
		nullableString(note), actor.AdminID, actor.SessionID); err != nil {
		return TransitionResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET fulfillment_status=$2, updated_at=now() WHERE id=$1`,
		cmd.OrderID, transition.to); err != nil {
		return TransitionResult{}, err
	}
	auditMetadata := mustJSON(map[string]any{
		"event": transition.event, "from": transition.from, "to": transition.to,
		"version": cmd.ExpectedVersion + 1, "settlementMode": settlement,
		"actorAdminId": actor.AdminID, "simulated": settlement == SettlementSimulated,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_audit_events (order_id, customer_id, action, metadata)
		VALUES ($1,$2,$3,$4::jsonb)`,
		cmd.OrderID, uuidString(current.CustomerUUID), transition.event, auditMetadata); err != nil {
		return TransitionResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TransitionResult{}, err
	}
	updated, err := s.readState(ctx, s.pool, cmd.OrderID)
	if err != nil {
		return TransitionResult{}, err
	}
	return TransitionResult{State: updated.State, Replayed: false}, nil
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
