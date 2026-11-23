package customer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("customer not found")

// Repository owns all customer SQL. Every statement is parameterized.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

type rowScanner interface {
	Scan(dest ...any) error
}

const customerColumns = `id, name, email, phone_e164, phone_display, password_hash, status, created_at, updated_at, last_login_at`

func scanCustomer(row rowScanner) (Customer, error) {
	var c Customer
	err := row.Scan(&c.ID, &c.Name, &c.Email, &c.PhoneE164, &c.PhoneDisplay, &c.PasswordHash,
		&c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ErrNotFound
	}
	return c, err
}

const sessionColumns = `id, customer_id, token_hash, created_at, expires_at, revoked_at, last_seen_at, created_ip, user_agent`

func scanSession(row rowScanner) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.CustomerID, &s.TokenHash, &s.CreatedAt, &s.ExpiresAt,
		&s.RevokedAt, &s.LastSeenAt, &s.CreatedIP, &s.UserAgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

func (r *Repository) FindByEmail(ctx context.Context, email string) (Customer, error) {
	return scanCustomer(r.pool.QueryRow(ctx,
		`SELECT `+customerColumns+` FROM customers WHERE email = $1`, NormalizeEmail(email)))
}

func (r *Repository) FindByID(ctx context.Context, id string) (Customer, error) {
	return scanCustomer(r.pool.QueryRow(ctx,
		`SELECT `+customerColumns+` FROM customers WHERE id = $1`, id))
}

func (r *Repository) CreateTx(ctx context.Context, tx pgx.Tx, in NormalizedRegistration, passwordHash string) (Customer, error) {
	const q = `
		INSERT INTO customers (name, email, phone_e164, phone_display, password_hash, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + customerColumns
	return scanCustomer(tx.QueryRow(ctx, q, in.Name, in.Email, in.PhoneE164, in.PhoneDisplay,
		passwordHash, StatusActive))
}

func (r *Repository) UpdateLastLoginTx(ctx context.Context, tx pgx.Tx, customerID string, at time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE customers SET last_login_at = $2, updated_at = now() WHERE id = $1`, customerID, at)
	return err
}

func (r *Repository) CreateSessionTx(ctx context.Context, tx pgx.Tx, customerID string, tokenHash []byte, expiresAt time.Time, createdIP, userAgent *string) (Session, error) {
	const q = `
		INSERT INTO customer_sessions (customer_id, token_hash, expires_at, created_ip, user_agent)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + sessionColumns
	return scanSession(tx.QueryRow(ctx, q, customerID, tokenHash, expiresAt, createdIP, userAgent))
}

// FindActiveSession returns a session only when it is unrevoked, unexpired and
// owned by an active customer. The customers table is joined in the same query.
func (r *Repository) FindActiveSession(ctx context.Context, tokenHash []byte) (Session, Customer, error) {
	const q = `
		SELECT s.id, s.customer_id, s.token_hash, s.created_at, s.expires_at, s.revoked_at, s.last_seen_at, s.created_ip, s.user_agent,
		       c.id, c.name, c.email, c.phone_e164, c.phone_display, c.password_hash, c.status, c.created_at, c.updated_at, c.last_login_at
		FROM customer_sessions s
		JOIN customers c ON c.id = s.customer_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > now()
		  AND c.status = 'active'`
	row := r.pool.QueryRow(ctx, q, tokenHash)
	var s Session
	var c Customer
	err := row.Scan(
		&s.ID, &s.CustomerID, &s.TokenHash, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt, &s.LastSeenAt, &s.CreatedIP, &s.UserAgent,
		&c.ID, &c.Name, &c.Email, &c.PhoneE164, &c.PhoneDisplay, &c.PasswordHash, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastLoginAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, Customer{}, ErrNotFound
	}
	return s, c, err
}

func (r *Repository) TouchSession(ctx context.Context, sessionID string, at time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE customer_sessions SET last_seen_at = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, at)
	return err
}

func (r *Repository) RevokeSessionTx(ctx context.Context, tx pgx.Tx, sessionID string, at time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE customer_sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) InsertAuditEvent(ctx context.Context, customerID *string, action string, metadata map[string]any) error {
	return r.insertAuditEvent(ctx, nil, customerID, action, metadata)
}

func (r *Repository) InsertAuditEventTx(ctx context.Context, tx pgx.Tx, customerID *string, action string, metadata map[string]any) error {
	return r.insertAuditEvent(ctx, tx, customerID, action, metadata)
}

func (r *Repository) insertAuditEvent(ctx context.Context, tx pgx.Tx, customerID *string, action string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal customer audit metadata: %w", err)
	}
	var id any
	if customerID != nil && *customerID != "" {
		id = *customerID
	}
	const q = `INSERT INTO customer_audit_events (customer_id, action, metadata) VALUES ($1, $2, $3::jsonb)`
	if tx != nil {
		_, err = tx.Exec(ctx, q, id, action, payload)
	} else {
		_, err = r.pool.Exec(ctx, q, id, action, payload)
	}
	return err
}
