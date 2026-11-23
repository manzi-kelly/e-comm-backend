package orders

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/product"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	errReferenceRace   = errors.New("order reference collision")
	errIdempotencyRace = errors.New("idempotency key collision")
	reservationRelease = "active_until_expiry_then_released_no_payment_deduction"
	referenceAlphabet  = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// Service is the only supported order-creation path. It reads authoritative
// prices from PostgreSQL inside a transaction and locks stock rows.
type Service struct {
	pool *pgxpool.Pool
	cfg  config.Config
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg config.Config) *Service {
	return &Service{pool: pool, cfg: cfg, now: time.Now}
}

// SetNow is a test hook for reservation-expiry scenarios.
func (s *Service) SetNow(now func() time.Time) { s.now = now }

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type RequestMeta struct {
	IP        string
	UserAgent string
}

type saleProduct struct {
	ID           string
	SKU          string
	NameEN       string
	NameRW       *string
	Condition    string
	PriceRWF     int64
	Stock        int64
	Reserved     int64
	Published    bool
	Deleted      bool
	Country      string
	Availability string
}

func generatedReference(at time.Time) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate order reference: %w", err)
	}
	chars := make([]byte, len(b))
	for i, v := range b {
		chars[i] = referenceAlphabet[int(v)%len(referenceAlphabet)]
	}
	return "AV-" + at.UTC().Format("20060102") + "-" + string(chars), nil
}

func truncateMeta(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 512 {
		return v[:512]
	}
	return v
}

func metaPointers(meta RequestMeta) (*string, *string) {
	ip := truncateMeta(meta.IP)
	ua := truncateMeta(meta.UserAgent)
	if ip == "" {
		ip = "unknown"
	}
	var uaPtr *string
	if ua != "" {
		uaPtr = &ua
	}
	return &ip, uaPtr
}

func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(parts, ",")
}

func fingerprintMatches(stored []byte, expected []byte) bool {
	return auth.TokensEqual(stored, expected)
}

func isConstraint(err error, name string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == name
}

// Create validates stock, calculates all money, and writes the order, item
// snapshots, reservations and audit row in one transaction.
func (s *Service) Create(ctx context.Context, customerID string, in NormalizedInput, idempotencyKey string, meta RequestMeta) (Order, bool, error) {
	if customerID == "" || !product.ValidID(customerID) || !ValidIdempotencyKey(idempotencyKey) || len(in.Fingerprint) != 32 {
		return Order{}, false, ErrValidation
	}
	for attempt := 0; attempt < 3; attempt++ {
		order, replay, err := s.createOnce(ctx, customerID, in, idempotencyKey, meta)
		if err == nil {
			return order, replay, nil
		}
		switch {
		case errors.Is(err, errReferenceRace):
			continue
		case errors.Is(err, errIdempotencyRace):
			existing, findErr := s.findByIdempotency(ctx, customerID, idempotencyKey)
			if findErr != nil {
				return Order{}, false, findErr
			}
			if !fingerprintMatches(existing.Fingerprint, in.Fingerprint) {
				return Order{}, false, ErrIdempotencyConflict
			}
			return existing.Order, true, nil
		default:
			return Order{}, false, err
		}
	}
	return Order{}, false, ErrConflict
}

type existingOrder struct {
	Order       Order
	Fingerprint []byte
}

func (s *Service) findByIdempotency(ctx context.Context, customerID, key string) (existingOrder, error) {
	var fingerprint []byte
	var id string
	err := s.pool.QueryRow(ctx,
		`SELECT id, request_fingerprint FROM orders WHERE customer_id=$1 AND idempotency_key=$2`,
		customerID, key).Scan(&id, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return existingOrder{}, ErrConflict
	}
	if err != nil {
		return existingOrder{}, err
	}
	order, err := s.readOrder(ctx, s.pool, "o.id = $1", id)
	return existingOrder{Order: order, Fingerprint: fingerprint}, err
}

func (s *Service) findByIdempotencyTx(ctx context.Context, tx pgx.Tx, customerID, key string) (existingOrder, error) {
	var fingerprint []byte
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id, request_fingerprint FROM orders WHERE customer_id=$1 AND idempotency_key=$2 FOR UPDATE`,
		customerID, key).Scan(&id, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return existingOrder{}, nil
	}
	if err != nil {
		return existingOrder{}, err
	}
	order, err := s.readOrder(ctx, tx, "o.id = $1", id)
	return existingOrder{Order: order, Fingerprint: fingerprint}, err
}

func (s *Service) createOnce(ctx context.Context, customerID string, in NormalizedInput, idempotencyKey string, meta RequestMeta) (Order, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Order{}, false, fmt.Errorf("begin order transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	existing, err := s.findByIdempotencyTx(ctx, tx, customerID, idempotencyKey)
	if err != nil {
		return Order{}, false, err
	}
	if existing.Order.ID != "" {
		if !fingerprintMatches(existing.Fingerprint, in.Fingerprint) {
			return Order{}, false, ErrIdempotencyConflict
		}
		return existing.Order, true, nil
	}

	ids := make([]string, 0, len(in.Items))
	seen := map[string]bool{}
	for _, item := range in.Items {
		if !seen[item.ProductID] {
			seen[item.ProductID] = true
			ids = append(ids, item.ProductID)
		}
	}
	inClause := placeholders(len(ids))
	lockArgs := make([]any, 0, len(ids))
	for _, id := range ids {
		lockArgs = append(lockArgs, id)
	}
	// Lock product and inventory rows in ID order so concurrent orders for the
	// same stock cannot both observe the same available quantity.
	_, err = tx.Exec(ctx,
		`SELECT p.id FROM products p JOIN inventory i ON i.product_id=p.id
		 WHERE p.id IN (`+inClause+`) ORDER BY p.id FOR UPDATE OF p, i`, lockArgs...)
	if err != nil {
		return Order{}, false, err
	}

	detailArgs := append([]any{}, lockArgs...)
	rows, err := tx.Query(ctx, `
		SELECT p.id, p.sku, p.name_en, p.name_rw, p.condition, p.price_rwf,
		       i.stock_quantity, i.availability, p.published, p.deleted_at IS NOT NULL, p.country,
		       COALESCE((SELECT SUM(r.quantity) FROM inventory_reservations r
		                 WHERE r.product_id=p.id AND r.status='active' AND r.expires_at > now()), 0)
		FROM products p JOIN inventory i ON i.product_id=p.id
		WHERE p.id IN (`+inClause+`)`, detailArgs...)
	if err != nil {
		return Order{}, false, err
	}
	catalogue := map[string]saleProduct{}
	for rows.Next() {
		var p saleProduct
		if err := rows.Scan(&p.ID, &p.SKU, &p.NameEN, &p.NameRW, &p.Condition, &p.PriceRWF,
			&p.Stock, &p.Availability, &p.Published, &p.Deleted, &p.Country, &p.Reserved); err != nil {
			rows.Close()
			return Order{}, false, err
		}
		catalogue[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Order{}, false, err
	}
	rows.Close()
	if len(catalogue) != len(ids) {
		return Order{}, false, ErrProductNotFound
	}

	type priced struct {
		product  saleProduct
		quantity int
		line     int64
	}
	pricedItems := make([]priced, 0, len(in.Items))
	var subtotal int64
	for _, requested := range in.Items {
		p := catalogue[requested.ProductID]
		if !p.Published || p.Deleted || p.Country != CountryRW || p.Availability != "available" {
			return Order{}, false, ErrProductNotFound
		}
		if p.Stock-p.Reserved < int64(requested.Quantity) {
			return Order{}, false, ErrStockUnavailable
		}
		if p.PriceRWF < 1 || p.PriceRWF > product.MaxInteger || int64(requested.Quantity) > product.MaxInteger/p.PriceRWF {
			return Order{}, false, ErrValidation
		}
		line := p.PriceRWF * int64(requested.Quantity)
		if line > product.MaxInteger || subtotal > product.MaxInteger-line {
			return Order{}, false, ErrValidation
		}
		subtotal += line
		pricedItems = append(pricedItems, priced{product: p, quantity: requested.Quantity, line: line})
	}
	if subtotal < 1 || subtotal > product.MaxInteger {
		return Order{}, false, ErrValidation
	}

	now := s.now()
	reference, err := generatedReference(now)
	if err != nil {
		return Order{}, false, err
	}
	expiresAt := now.Add(s.cfg.Orders.ReservationTTL)
	ip, ua := metaPointers(meta)
	var cell, instructions any
	if in.Cell != "" {
		cell = in.Cell
	}
	if in.Instructions != "" {
		instructions = in.Instructions
	}

	var orderID string
	const insertOrder = `
		INSERT INTO orders (
			reference, customer_id, idempotency_key, request_fingerprint,
			customer_name, customer_email, customer_phone_e164, customer_phone_display,
			country, province, district, sector, cell, address_line, instructions,
			subtotal_rwf, delivery_fee_rwf, delivery_fee_status, total_rwf,
			payment_status, fulfillment_status, created_ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		RETURNING id`
	err = tx.QueryRow(ctx, insertOrder,
		reference, customerID, idempotencyKey, in.Fingerprint,
		in.FullName, in.Email, in.PhoneE164, in.PhoneDisplay,
		CountryRW, in.Province, in.District, in.Sector, cell, in.AddressLine, instructions,
		subtotal, int64(0), FeeUnavailable, subtotal,
		PaymentPending, FulfillmentPending, ip, ua).Scan(&orderID)
	if err != nil {
		if isConstraint(err, "orders_reference_key") {
			return Order{}, false, errReferenceRace
		}
		if isConstraint(err, "orders_customer_idempotency_key") {
			return Order{}, false, errIdempotencyRace
		}
		return Order{}, false, fmt.Errorf("insert order: %w", err)
	}

	for i, item := range pricedItems {
		_, err = tx.Exec(ctx, `
			INSERT INTO order_items (
				order_id, product_id, snapshot_name_en, snapshot_name_rw, snapshot_sku,
				snapshot_condition, quantity, unit_price_rwf, line_total_rwf)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			orderID, item.product.ID, item.product.NameEN, item.product.NameRW, item.product.SKU,
			item.product.Condition, item.quantity, item.product.PriceRWF, item.line)
		if err != nil {
			return Order{}, false, fmt.Errorf("insert order item %d: %w", i, err)
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO inventory_reservations (product_id, order_id, quantity, status, expires_at)
			VALUES ($1,$2,$3,'active',$4)`, item.product.ID, orderID, item.quantity, expiresAt); err != nil {
			return Order{}, false, fmt.Errorf("reserve stock: %w", err)
		}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO order_audit_events (order_id, customer_id, action, metadata)
		VALUES ($1,$2,'order.created',jsonb_build_object('reference',$3::text,'item_count',$4::int,'total_rwf',$5::bigint))`,
		orderID, customerID, reference, len(pricedItems), subtotal)
	if err != nil {
		return Order{}, false, fmt.Errorf("record order audit: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO customer_audit_events (customer_id, action, metadata)
		VALUES ($1,'customer.order.created',jsonb_build_object('order_id',$2::text,'reference',$3::text))`,
		customerID, orderID, reference)
	if err != nil {
		return Order{}, false, fmt.Errorf("record customer order audit: %w", err)
	}

	order, err := s.readOrder(ctx, tx, "o.id = $1", orderID)
	if err != nil {
		return Order{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isConstraint(err, "orders_reference_key") {
			return Order{}, false, errReferenceRace
		}
		if isConstraint(err, "orders_customer_idempotency_key") {
			return Order{}, false, errIdempotencyRace
		}
		return Order{}, false, fmt.Errorf("commit order: %w", err)
	}
	return order, false, nil
}

const orderSelectColumns = `
	o.id, o.reference, o.customer_name, o.customer_email, o.customer_phone_e164, o.customer_phone_display,
	o.country, o.province, o.district, o.sector, o.cell, o.address_line, o.instructions,
	o.subtotal_rwf, o.delivery_fee_rwf, o.delivery_fee_status, o.total_rwf,
	o.payment_status, o.fulfillment_status, o.created_at,
	(SELECT min(r.expires_at) FROM inventory_reservations r
	 WHERE r.order_id=o.id AND r.status='active' AND r.expires_at > now())`

func (s *Service) readOrder(ctx context.Context, q queryer, where string, args ...any) (Order, error) {
	var o Order
	var fee int64
	var cell, instructions *string
	var reservationExpiry *time.Time
	err := q.QueryRow(ctx, `SELECT `+orderSelectColumns+` FROM orders o WHERE `+where, args...).Scan(
		&o.ID, &o.Reference, &o.Customer.FullName, &o.Customer.Email, &o.Customer.PhoneE164, &o.Customer.Phone,
		&o.Delivery.Country, &o.Delivery.Province, &o.Delivery.District, &o.Delivery.Sector, &cell, &o.Delivery.AddressLine, &instructions,
		&o.SubtotalRWF, &fee, &o.DeliveryFeeStatus, &o.TotalRWF,
		&o.PaymentStatus, &o.Status, &o.OrderedAt, &reservationExpiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if o.DeliveryFeeStatus != FeeUnavailable {
		o.DeliveryFeeRWF = &fee
	}
	o.Delivery.Cell = cell
	o.Delivery.Instructions = instructions
	o.Currency = CurrencyRWF
	o.AmountDueRWF = o.TotalRWF
	o.PaidRWF = 0
	o.PaymentTerm = FullPaymentTerm
	o.Timeline = []TimelineEntry{{Step: "placed", At: &o.OrderedAt}}
	o.Reservation = Reservation{Status: "expired", ReleasePolicy: reservationRelease}
	if reservationExpiry != nil && reservationExpiry.After(s.now()) {
		o.Reservation.Status = "active"
		o.Reservation.ExpiresAt = reservationExpiry
	}

	rows, err := q.Query(ctx, `
		SELECT id, product_id, snapshot_name_en, snapshot_name_rw, snapshot_sku,
		       snapshot_condition, quantity, unit_price_rwf, line_total_rwf
		FROM order_items WHERE order_id=$1 ORDER BY created_at, id`, o.ID)
	if err != nil {
		return Order{}, err
	}
	defer rows.Close()
	o.Items = []ItemSnapshot{}
	for rows.Next() {
		var item ItemSnapshot
		if err := rows.Scan(&item.ID, &item.ProductID, &item.Name.EN, &item.Name.RW, &item.SKU,
			&item.Condition, &item.Quantity, &item.UnitPriceRWF, &item.LineTotalRWF); err != nil {
			return Order{}, err
		}
		o.Items = append(o.Items, item)
	}
	return o, rows.Err()
}

// Get returns an order only when it belongs to the authenticated customer.
func (s *Service) Get(ctx context.Context, customerID, orderID string) (Order, error) {
	if !product.ValidID(customerID) || !product.ValidID(orderID) {
		return Order{}, ErrNotFound
	}
	return s.readOrder(ctx, s.pool, "o.id = $1 AND o.customer_id = $2", orderID, customerID)
}

// List returns only the authenticated customer's own orders.
func (s *Service) List(ctx context.Context, customerID string, page, limit int) ([]Summary, bool, error) {
	if !product.ValidID(customerID) || page < 1 || page > 1000 || limit < 1 || limit > 100 {
		return nil, false, ErrValidation
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.reference, o.created_at, o.payment_status, o.fulfillment_status, o.total_rwf,
		       (SELECT count(*) FROM order_items i WHERE i.order_id=o.id)
		FROM orders o WHERE o.customer_id=$1
		ORDER BY o.created_at DESC, o.id DESC LIMIT $2 OFFSET $3`,
		customerID, limit+1, (page-1)*limit)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []Summary{}
	for rows.Next() {
		var item Summary
		if err := rows.Scan(&item.ID, &item.Reference, &item.OrderedAt, &item.PaymentStatus,
			&item.Status, &item.TotalRWF, &item.ItemCount); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasNext := len(items) > limit
	if hasNext {
		items = items[:limit]
	}
	return items, hasNext, nil
}
