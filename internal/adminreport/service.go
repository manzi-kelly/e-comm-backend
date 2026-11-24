package adminreport

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"autivashop/backend/internal/fulfillment"
	"autivashop/backend/internal/product"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service is read-only. It is separate from order creation and payment state
// transitions so an Admin read can never mutate authoritative records.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

const reservationReleasePolicy = "active_until_expiry_then_released_no_payment_deduction"

var (
	datePattern           = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	orderReferencePattern = regexp.MustCompile(`^AV-[0-9]{8}-[A-Z0-9]{12}$`)
)

func maskPhone(e164 string) string {
	if len(e164) < 10 {
		return "••••"
	}
	return e164[:6] + "••••" + e164[len(e164)-3:]
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

func validPaymentStatus(value string) bool {
	switch value {
	case PaymentPending, PaymentPendingAlt, PaymentPaid, PaymentFailed, PaymentCancelled:
		return true
	default:
		return false
	}
}

func validFulfillmentStatus(value string) bool {
	return value == FulfillmentNoStock || fulfillment.ValidStatus(value)
}

const orderRecordColumns = `
	o.id, o.customer_id, o.reference, o.created_at, o.payment_status, o.fulfillment_status,
	o.customer_name, o.customer_email, o.customer_phone_e164, o.customer_phone_display,
	o.country, o.province, o.district, o.sector, o.cell, o.address_line, o.instructions,
	o.subtotal_rwf, o.delivery_fee_rwf, o.delivery_fee_status, o.total_rwf,
	(SELECT count(*) FROM order_items i WHERE i.order_id=o.id),
	latest.state, latest.is_mock, latest.provider_name, latest.created_at,
	res.status, res.expires_at
FROM orders o
LEFT JOIN LATERAL (
	SELECT pa.state, pa.is_mock, pa.provider_name, pa.created_at
	FROM payment_attempts pa WHERE pa.order_id=o.id
	ORDER BY pa.created_at DESC, pa.id DESC LIMIT 1
) latest ON true
LEFT JOIN LATERAL (
	SELECT r.status, r.expires_at
	FROM inventory_reservations r WHERE r.order_id=o.id
	ORDER BY r.created_at DESC, r.id DESC LIMIT 1
) res ON true`

type orderRecord struct {
	ID                   string
	CustomerID           string
	Reference            string
	OrderedAt            time.Time
	PaymentStatus        string
	FulfillmentStatus    string
	CustomerName         string
	CustomerEmail        string
	CustomerPhoneE164    string
	CustomerPhoneDisplay string
	Country              string
	Province             string
	District             string
	Sector               string
	Cell                 *string
	AddressLine          string
	Instructions         *string
	SubtotalRWF          int64
	DeliveryFeeRWF       int64
	DeliveryFeeStatus    string
	TotalRWF             int64
	ItemCount            int
	LatestState          *string
	LatestIsMock         *bool
	LatestProvider       *string
	LatestCreatedAt      *time.Time
	ReservationStatus    *string
	ReservationExpiry    *time.Time
}

func (r orderRecord) summary() OrderSummary {
	summary := OrderSummary{
		ID:                r.ID,
		Reference:         r.Reference,
		CustomerID:        r.CustomerID,
		OrderedAt:         r.OrderedAt,
		OrderStatus:       r.FulfillmentStatus,
		PaymentStatus:     r.PaymentStatus,
		FulfillmentStatus: r.FulfillmentStatus,
		CustomerName:      r.CustomerName,
		CustomerEmail:     r.CustomerEmail,
		CustomerPhone:     maskPhone(r.CustomerPhoneE164),
		Currency:          CurrencyRWF,
		SubtotalRWF:       r.SubtotalRWF,
		TotalRWF:          r.TotalRWF,
		ItemCount:         r.ItemCount,
	}
	if r.LatestState != nil && r.LatestCreatedAt != nil {
		attempt := &Attempt{
			State:     *r.LatestState,
			CreatedAt: *r.LatestCreatedAt,
		}
		if r.LatestIsMock != nil {
			attempt.IsMock = *r.LatestIsMock
		}
		if r.LatestProvider != nil {
			attempt.Provider = *r.LatestProvider
		}
		summary.LatestAttempt = attempt
	}
	return summary
}

func (r orderRecord) contact() Contact {
	return Contact{
		FullName:  r.CustomerName,
		Email:     r.CustomerEmail,
		Phone:     maskPhone(r.CustomerPhoneE164),
		PhoneE164: maskPhone(r.CustomerPhoneE164),
	}
}

func (r orderRecord) delivery() Delivery {
	return Delivery{
		Country:      r.Country,
		Province:     r.Province,
		District:     r.District,
		Sector:       r.Sector,
		Cell:         r.Cell,
		AddressLine:  r.AddressLine,
		Instructions: r.Instructions,
	}
}

func (r orderRecord) fulfillment() Fulfillment {
	status := "none"
	if r.ReservationStatus != nil {
		status = *r.ReservationStatus
	}
	return Fulfillment{
		Status:               r.FulfillmentStatus,
		ReservationStatus:    status,
		ReservationExpiresAt: r.ReservationExpiry,
		ReleasePolicy:        reservationReleasePolicy,
	}
}

func scanOrderRecord(rows pgx.Rows) (orderRecord, error) {
	var r orderRecord
	var customerID pgtype.UUID
	if err := rows.Scan(
		&r.ID, &customerID, &r.Reference, &r.OrderedAt, &r.PaymentStatus, &r.FulfillmentStatus,
		&r.CustomerName, &r.CustomerEmail, &r.CustomerPhoneE164, &r.CustomerPhoneDisplay,
		&r.Country, &r.Province, &r.District, &r.Sector, &r.Cell, &r.AddressLine, &r.Instructions,
		&r.SubtotalRWF, &r.DeliveryFeeRWF, &r.DeliveryFeeStatus, &r.TotalRWF, &r.ItemCount,
		&r.LatestState, &r.LatestIsMock, &r.LatestProvider, &r.LatestCreatedAt,
		&r.ReservationStatus, &r.ReservationExpiry); err != nil {
		return orderRecord{}, err
	}
	if customerID.Valid {
		b := customerID.Bytes
		r.CustomerID = fmt.Sprintf("%x-%x-%x-%x-%x",
			b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return r, nil
}

func readOrderRecord(ctx context.Context, q queryer, where string, args ...any) (orderRecord, error) {
	rows, err := q.Query(ctx, `SELECT `+orderRecordColumns+` WHERE `+where, args...)
	if err != nil {
		return orderRecord{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return orderRecord{}, err
		}
		return orderRecord{}, ErrNotFound
	}
	r, err := scanOrderRecord(rows)
	if err != nil {
		return orderRecord{}, err
	}
	return r, rows.Err()
}

func (f *OrderFilter) normalize() error {
	if f.Page < 1 || f.Page > 1000 || f.Limit < 1 || f.Limit > 100 {
		return ErrValidation
	}
	if len(f.Search) > 200 || (f.OrderReference != "" && !orderReferencePattern.MatchString(f.OrderReference)) {
		return ErrValidation
	}
	if !validDate(f.FromDate) || !validDate(f.ToDate) {
		return ErrValidation
	}
	if f.OrderStatus != "" {
		if !validFulfillmentStatus(f.OrderStatus) {
			return ErrValidation
		}
		if f.FulfillmentStatus != "" && f.FulfillmentStatus != f.OrderStatus {
			return ErrValidation
		}
		f.FulfillmentStatus = f.OrderStatus
	}
	if f.LocalDelivery != "" {
		if f.LocalDelivery == "not-recorded" {
			f.LocalDelivery = FulfillmentPending
		}
		if !validFulfillmentStatus(f.LocalDelivery) {
			return ErrValidation
		}
		if f.FulfillmentStatus != "" && f.FulfillmentStatus != f.LocalDelivery {
			return ErrValidation
		}
		f.FulfillmentStatus = f.LocalDelivery
	}
	if f.PaymentStatus != "" && !validPaymentStatus(f.PaymentStatus) {
		return ErrValidation
	}
	if f.FulfillmentStatus != "" && !validFulfillmentStatus(f.FulfillmentStatus) {
		return ErrValidation
	}
	if f.Sort == "" {
		f.Sort = "newest"
	}
	switch f.Sort {
	case "newest", "oldest", "total-asc", "total-desc":
	default:
		return ErrValidation
	}
	return nil
}

func (f OrderFilter) where() (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if f.Search != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Search)
		pattern := "%" + escaped + "%"
		searchClauses := make([]string, 0, 6)
		for _, column := range []string{
			"o.reference", "o.id::text", "o.customer_name",
			"o.customer_email", "o.customer_phone_e164", "o.customer_phone_display",
		} {
			args = append(args, pattern)
			searchClauses = append(searchClauses, fmt.Sprintf(`%s ILIKE $%d ESCAPE '\'`, column, len(args)))
		}
		clauses = append(clauses, "("+strings.Join(searchClauses, " OR ")+")")
	}
	if f.OrderReference != "" {
		add("o.reference = $%d", f.OrderReference)
	}
	if f.PaymentStatus != "" {
		add("o.payment_status = $%d", f.PaymentStatus)
	}
	if f.FulfillmentStatus != "" {
		add("o.fulfillment_status = $%d", f.FulfillmentStatus)
	}
	if f.FromDate != "" {
		add("o.created_at >= $%d::date", f.FromDate)
	}
	if f.ToDate != "" {
		add("o.created_at < $%d::date + interval '1 day'", f.ToDate)
	}
	return strings.Join(clauses, " AND "), args
}

func sortClause(sort string) string {
	switch sort {
	case "oldest":
		return "o.created_at ASC, o.id ASC"
	case "total-asc":
		return "o.total_rwf ASC, o.id ASC"
	case "total-desc":
		return "o.total_rwf DESC, o.id DESC"
	default:
		return "o.created_at DESC, o.id DESC"
	}
}

// ListOrders returns a paginated Admin projection of all orders. Filters are
// validated before they are converted into parameterized SQL predicates.
func (s *Service) ListOrders(ctx context.Context, f OrderFilter) (OrderPage, error) {
	if err := f.normalize(); err != nil {
		return OrderPage{}, err
	}
	where, args := f.where()
	limitIndex := len(args) + 1
	offsetIndex := limitIndex + 1
	args = append(args, f.Limit+1, (f.Page-1)*f.Limit)
	query := `SELECT ` + orderRecordColumns + ` WHERE ` + where +
		` ORDER BY ` + sortClause(f.Sort) +
		` LIMIT $` + fmt.Sprint(limitIndex) + ` OFFSET $` + fmt.Sprint(offsetIndex)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return OrderPage{}, err
	}
	defer rows.Close()
	items := []OrderSummary{}
	for rows.Next() {
		r, err := scanOrderRecord(rows)
		if err != nil {
			return OrderPage{}, err
		}
		items = append(items, r.summary())
	}
	if err := rows.Err(); err != nil {
		return OrderPage{}, err
	}
	hasNext := len(items) > f.Limit
	if hasNext {
		items = items[:f.Limit]
	}
	return OrderPage{Items: items, Page: f.Page, Limit: f.Limit, HasNext: hasNext}, nil
}

// GetOrder returns the composite Admin detail for one order.
func (s *Service) GetOrder(ctx context.Context, orderID string) (OrderDetail, error) {
	if !product.ValidID(orderID) {
		return OrderDetail{}, ErrNotFound
	}
	r, err := readOrderRecord(ctx, s.pool, "o.id = $1", orderID)
	if err != nil {
		return OrderDetail{}, err
	}
	items, err := s.readItems(ctx, orderID)
	if err != nil {
		return OrderDetail{}, err
	}
	attempts, err := s.readAttempts(ctx, "pa.order_id = $1", orderID)
	if err != nil {
		return OrderDetail{}, err
	}
	summary := summarizeAttempts(attempts)
	return OrderDetail{
		Order:                 r.summary(),
		Contact:               r.contact(),
		Delivery:              r.delivery(),
		Items:                 items,
		Payment:               Payment{Status: r.PaymentStatus, AttemptSummary: summary},
		Fulfillment:           r.fulfillment(),
		PaymentAttempts:       attempts,
		PaymentAttemptSummary: summary,
	}, nil
}

// GetItems, GetContact, GetDelivery, GetPayment and GetFulfillment are the
// narrow Admin subresources. They all use the same owner-independent order
// lookup but expose only the named projection.
func (s *Service) GetItems(ctx context.Context, orderID string) ([]Item, error) {
	if !product.ValidID(orderID) {
		return nil, ErrNotFound
	}
	if err := s.orderExists(ctx, orderID); err != nil {
		return nil, err
	}
	return s.readItems(ctx, orderID)
}

func (s *Service) GetContact(ctx context.Context, orderID string) (Contact, error) {
	if !product.ValidID(orderID) {
		return Contact{}, ErrNotFound
	}
	r, err := readOrderRecord(ctx, s.pool, "o.id = $1", orderID)
	if err != nil {
		return Contact{}, err
	}
	return r.contact(), nil
}

func (s *Service) GetDelivery(ctx context.Context, orderID string) (Delivery, error) {
	if !product.ValidID(orderID) {
		return Delivery{}, ErrNotFound
	}
	r, err := readOrderRecord(ctx, s.pool, "o.id = $1", orderID)
	if err != nil {
		return Delivery{}, err
	}
	return r.delivery(), nil
}

func (s *Service) GetPayment(ctx context.Context, orderID string) (Payment, error) {
	if !product.ValidID(orderID) {
		return Payment{}, ErrNotFound
	}
	r, err := readOrderRecord(ctx, s.pool, "o.id = $1", orderID)
	if err != nil {
		return Payment{}, err
	}
	attempts, err := s.readAttempts(ctx, "pa.order_id = $1", orderID)
	if err != nil {
		return Payment{}, err
	}
	return Payment{Status: r.PaymentStatus, AttemptSummary: summarizeAttempts(attempts)}, nil
}

func (s *Service) GetFulfillment(ctx context.Context, orderID string) (Fulfillment, error) {
	if !product.ValidID(orderID) {
		return Fulfillment{}, ErrNotFound
	}
	r, err := readOrderRecord(ctx, s.pool, "o.id = $1", orderID)
	if err != nil {
		return Fulfillment{}, err
	}
	return r.fulfillment(), nil
}

func (s *Service) GetAttempts(ctx context.Context, orderID string, page, limit int) (AttemptPage, error) {
	if !product.ValidID(orderID) || page < 1 || page > 1000 || limit < 1 || limit > 100 {
		return AttemptPage{}, ErrValidation
	}
	if err := s.orderExists(ctx, orderID); err != nil {
		return AttemptPage{}, err
	}
	attempts, err := s.readAttempts(ctx, "pa.order_id = $1", orderID)
	if err != nil {
		return AttemptPage{}, err
	}
	summary := summarizeAttempts(attempts)
	start := (page - 1) * limit
	if start > len(attempts) {
		start = len(attempts)
	}
	end := start + limit
	if end > len(attempts) {
		end = len(attempts)
	}
	hasNext := end < len(attempts)
	return AttemptPage{Items: attempts[start:end], Page: page, Limit: limit, HasNext: hasNext, Summary: summary}, nil
}

func (s *Service) GetAttemptSummary(ctx context.Context, orderID string) (AttemptSummary, error) {
	if !product.ValidID(orderID) {
		return AttemptSummary{}, ErrNotFound
	}
	if err := s.orderExists(ctx, orderID); err != nil {
		return AttemptSummary{}, err
	}
	attempts, err := s.readAttempts(ctx, "pa.order_id = $1", orderID)
	if err != nil {
		return AttemptSummary{}, err
	}
	return summarizeAttempts(attempts), nil
}

func (s *Service) orderExists(ctx context.Context, orderID string) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE id=$1)`, orderID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func (s *Service) readItems(ctx context.Context, orderID string) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, product_id, snapshot_sku, snapshot_name_en, snapshot_name_rw, snapshot_condition,
		       quantity, unit_price_rwf, line_total_rwf
		FROM order_items WHERE order_id=$1 ORDER BY created_at, id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.ProductID, &item.SKU, &item.NameEN, &item.NameRW,
			&item.Condition, &item.Quantity, &item.UnitPriceRWF, &item.LineTotalRWF); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

const attemptColumns = `
	pa.id, pa.order_id, pa.order_reference, pa.provider_name, pa.provider_environment, pa.is_mock,
	pa.state, pa.amount_rwf, pa.currency, pa.customer_phone_e164, pa.network, pa.payment_method,
	pa.provider_reference, pa.prompt_status, pa.failure_code, pa.failure_message,
	pa.stock_outcome, pa.created_at, pa.updated_at, pa.completed_at`

func (s *Service) readAttempts(ctx context.Context, where string, args ...any) ([]Attempt, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+attemptColumns+`
		FROM payment_attempts pa WHERE `+where+`
		ORDER BY pa.created_at DESC, pa.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []Attempt{}
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.ID, &a.OrderID, &a.OrderReference, &a.Provider, &a.Environment,
			&a.IsMock, &a.State, &a.AmountRWF, &a.Currency, &a.PhoneE164, &a.Network,
			&a.PaymentMethod, &a.ProviderReference, &a.PromptStatus, &a.FailureCode,
			&a.FailureMessage, &a.StockOutcome, &a.CreatedAt, &a.UpdatedAt, &a.CompletedAt); err != nil {
			return nil, err
		}
		a.Phone = maskPhone(a.PhoneE164)
		a.PhoneE164 = maskPhone(a.PhoneE164)
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range attempts {
		transitions, err := s.readTransitions(ctx, attempts[i].ID)
		if err != nil {
			return nil, err
		}
		attempts[i].Transitions = transitions
	}
	return attempts, nil
}

func (s *Service) readTransitions(ctx context.Context, attemptID string) ([]Transition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT from_state, to_state, event_type, actor, delivery_id, occurred_at
		FROM payment_transitions WHERE attempt_id=$1 ORDER BY occurred_at, id`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Transition{}
	for rows.Next() {
		var tr Transition
		if err := rows.Scan(&tr.FromState, &tr.ToState, &tr.EventType, &tr.Actor, &tr.DeliveryID,
			&tr.OccurredAt); err != nil {
			return nil, err
		}
		items = append(items, tr)
	}
	return items, rows.Err()
}

func summarizeAttempts(attempts []Attempt) AttemptSummary {
	summary := AttemptSummary{MockRevenueIsReal: false}
	for _, a := range attempts {
		summary.TotalAttempts++
		switch a.State {
		case AttemptPending:
			summary.PendingAttempts++
		case AttemptSucceeded:
			summary.SucceededAttempts++
			if a.IsMock {
				summary.MockSucceededAttempts++
			} else {
				summary.LiveSucceededAttempts++
			}
		case AttemptFailed:
			summary.FailedAttempts++
		case AttemptCancelled:
			summary.CancelledAttempts++
		}
	}
	if len(attempts) > 0 {
		summary.LatestAttempt = &attempts[0]
	}
	return summary
}

// Dashboard reports authoritative counts. Real revenue is counted only for
// succeeded non-mock attempts with a verified payment event, once per order.
func (s *Service) Dashboard(ctx context.Context, recentLimit int) (Dashboard, error) {
	if recentLimit < 1 || recentLimit > 20 {
		return Dashboard{}, ErrValidation
	}
	d := Dashboard{
		GeneratedAt:       time.Now().UTC(),
		RevenuePolicy:     RevenueAccountingRule,
		MockRevenueIsReal: false,
		FulfillmentCounts: map[string]int64{
			FulfillmentPending: 0, FulfillmentNoStock: 0, FulfillmentProcessing: 0,
			FulfillmentReady: 0, FulfillmentOut: 0, FulfillmentDelivered: 0,
		},
		PaymentStatusCounts: map[string]int64{
			PaymentPending: 0, PaymentPendingAlt: 0, PaymentPaid: 0,
			PaymentFailed: 0, PaymentCancelled: 0,
		},
		RecentOrders: []RecentOrder{},
	}
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM orders),
			(SELECT count(*) FROM orders WHERE payment_status IN ('pending_payment','payment_pending')),
			(SELECT count(DISTINCT pa.order_id) FROM payment_attempts pa
			 WHERE pa.state='succeeded' AND pa.is_mock),
			(SELECT count(*) FROM payment_attempts WHERE state='failed'),
			(SELECT count(DISTINCT order_id) FROM payment_attempts WHERE state='failed'),
			COALESCE((SELECT count(*) FROM payment_attempts pa
			 WHERE pa.state='succeeded' AND NOT pa.is_mock
			   AND EXISTS (SELECT 1 FROM payment_events pe
			               WHERE pe.attempt_id=pa.id AND pe.verification_status='verified')),0),
			(SELECT count(DISTINCT pa.order_id) FROM payment_attempts pa
			 WHERE pa.state='succeeded' AND NOT pa.is_mock
			   AND EXISTS (SELECT 1 FROM payment_events pe
			               WHERE pe.attempt_id=pa.id AND pe.verification_status='verified')),
			COALESCE((SELECT SUM(total_rwf) FROM (
				SELECT DISTINCT ON (pa.order_id) pa.order_id, o.total_rwf
				FROM payment_attempts pa JOIN orders o ON o.id=pa.order_id
				WHERE pa.state='succeeded' AND NOT pa.is_mock
				  AND EXISTS (SELECT 1 FROM payment_events pe
				              WHERE pe.attempt_id=pa.id AND pe.verification_status='verified')
				ORDER BY pa.order_id, pa.completed_at DESC, pa.id DESC
			) verified_orders),0),
			COALESCE((SELECT SUM(total_rwf) FROM (
				SELECT DISTINCT ON (pa.order_id) pa.order_id, o.total_rwf
				FROM payment_attempts pa JOIN orders o ON o.id=pa.order_id
				WHERE pa.state='succeeded' AND pa.is_mock
				ORDER BY pa.order_id, pa.completed_at DESC, pa.id DESC
			) mock_orders),0)`).Scan(
		&d.TotalOrders, &d.PendingPaymentOrders, &d.MockSuccessfulOrders,
		&d.FailedPayments, &d.FailedPaymentOrders, &d.LiveVerifiedAttempts,
		&d.RealVerifiedOrders, &d.RealVerifiedRevenueRWF, &d.MockSettledAmountRWF)
	if err != nil {
		return Dashboard{}, err
	}
	if err := s.countGroups(ctx, &d); err != nil {
		return Dashboard{}, err
	}
	recent, err := s.readRecentOrders(ctx, recentLimit)
	if err != nil {
		return Dashboard{}, err
	}
	d.RecentOrders = recent
	return d, nil
}

func (s *Service) countGroups(ctx context.Context, d *Dashboard) error {
	rows, err := s.pool.Query(ctx, `SELECT payment_status, count(*) FROM orders GROUP BY payment_status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return err
		}
		d.PaymentStatusCounts[status] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT fulfillment_status, count(*) FROM orders GROUP BY fulfillment_status`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		d.FulfillmentCounts[status] = count
	}
	return rows.Err()
}

func (s *Service) readRecentOrders(ctx context.Context, limit int) ([]RecentOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.reference, o.created_at, o.payment_status, o.fulfillment_status,
		       o.customer_name, o.customer_phone_e164, o.total_rwf,
		       (SELECT count(*) FROM order_items i WHERE i.order_id=o.id)
		FROM orders o ORDER BY o.created_at DESC, o.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []RecentOrder{}
	for rows.Next() {
		var item RecentOrder
		var phone string
		if err := rows.Scan(&item.ID, &item.Reference, &item.OrderedAt, &item.PaymentStatus,
			&item.FulfillmentStatus, &item.CustomerName, &phone, &item.TotalRWF,
			&item.ItemCount); err != nil {
			return nil, err
		}
		item.OrderStatus = item.FulfillmentStatus
		item.CustomerPhone = maskPhone(phone)
		items = append(items, item)
	}
	return items, rows.Err()
}
