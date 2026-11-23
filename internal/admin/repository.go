package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// Repository provides PostgreSQL access for administrator authentication data.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (r *Repository) FindUserByEmail(ctx context.Context, email string) (User, error) {
	const q = `
		SELECT id, email, password_hash, display_name, role, is_active, created_at, updated_at, last_login_at
		FROM admin_users
		WHERE lower(email) = lower($1)`
	row := r.pool.QueryRow(ctx, q, NormalizeEmail(email))
	return scanUser(row)
}

func (r *Repository) FindUserByID(ctx context.Context, id string) (User, error) {
	const q = `
		SELECT id, email, password_hash, display_name, role, is_active, created_at, updated_at, last_login_at
		FROM admin_users
		WHERE id = $1`
	row := r.pool.QueryRow(ctx, q, id)
	return scanUser(row)
}

func (r *Repository) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (r *Repository) CreateUser(ctx context.Context, email, passwordHash, displayName string, role Role) (User, error) {
	const q = `
		INSERT INTO admin_users (email, password_hash, display_name, role, is_active)
		VALUES ($1, $2, $3, $4, TRUE)
		RETURNING id, email, password_hash, display_name, role, is_active, created_at, updated_at, last_login_at`
	row := r.pool.QueryRow(ctx, q, NormalizeEmail(email), passwordHash, strings.TrimSpace(displayName), string(role))
	return scanUser(row)
}

func (r *Repository) CreateUserTx(ctx context.Context, tx pgx.Tx, email, passwordHash, displayName string, role Role) (User, error) {
	const q = `
		INSERT INTO admin_users (email, password_hash, display_name, role, is_active)
		VALUES ($1, $2, $3, $4, TRUE)
		RETURNING id, email, password_hash, display_name, role, is_active, created_at, updated_at, last_login_at`
	row := tx.QueryRow(ctx, q, NormalizeEmail(email), passwordHash, strings.TrimSpace(displayName), string(role))
	return scanUser(row)
}

func (r *Repository) CountUsersTx(ctx context.Context, tx pgx.Tx) (int64, error) {
	// Serialize the bootstrap emptiness check with administrator inserts so
	// concurrent bootstrap commands cannot both create an initial account.
	if _, err := tx.Exec(ctx, `LOCK TABLE admin_users IN EXCLUSIVE MODE`); err != nil {
		return 0, err
	}
	var n int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (r *Repository) UpdateLastLogin(ctx context.Context, userID string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE admin_users SET last_login_at = $2, updated_at = now() WHERE id = $1`, userID, at)
	return err
}

func (r *Repository) CreateSession(ctx context.Context, adminUserID string, tokenHash []byte, expiresAt time.Time, createdIP, userAgent *string) (Session, error) {
	return r.CreateSessionTx(ctx, nil, adminUserID, tokenHash, expiresAt, createdIP, userAgent)
}

func (r *Repository) CreateSessionTx(ctx context.Context, tx pgx.Tx, adminUserID string, tokenHash []byte, expiresAt time.Time, createdIP, userAgent *string) (Session, error) {
	const q = `
		INSERT INTO admin_sessions (admin_user_id, token_hash, expires_at, created_ip, user_agent)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, admin_user_id, token_hash, created_at, expires_at, revoked_at, last_seen_at, created_ip, user_agent`
	var row pgx.Row
	if tx != nil {
		row = tx.QueryRow(ctx, q, adminUserID, tokenHash, expiresAt, createdIP, userAgent)
	} else {
		row = r.pool.QueryRow(ctx, q, adminUserID, tokenHash, expiresAt, createdIP, userAgent)
	}
	return scanSession(row)
}

// FindActiveSession returns a session joined with its active administrator if the token is valid, unexpired, and unrevoked.
func (r *Repository) FindActiveSession(ctx context.Context, tokenHash []byte) (Session, User, error) {
	const q = `
		SELECT
			s.id, s.admin_user_id, s.token_hash, s.created_at, s.expires_at, s.revoked_at, s.last_seen_at, s.created_ip, s.user_agent,
			u.id, u.email, u.password_hash, u.display_name, u.role, u.is_active, u.created_at, u.updated_at, u.last_login_at
		FROM admin_sessions s
		JOIN admin_users u ON u.id = s.admin_user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > now()
		  AND u.is_active = TRUE`
	row := r.pool.QueryRow(ctx, q, tokenHash)
	var s Session
	var u User
	var role string
	err := row.Scan(
		&s.ID, &s.AdminUserID, &s.TokenHash, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt, &s.LastSeenAt, &s.CreatedIP, &s.UserAgent,
		&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &role, &u.IsActive, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, User{}, ErrNotFound
		}
		return Session{}, User{}, err
	}
	u.Role = Role(role)
	return s, u, nil
}

func (r *Repository) TouchSession(ctx context.Context, sessionID string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, at)
	return err
}

func (r *Repository) RevokeSession(ctx context.Context, sessionID string, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE admin_sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, sessionID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) RevokeAllSessionsForUser(ctx context.Context, userID string, at time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE admin_sessions SET revoked_at = $2 WHERE admin_user_id = $1 AND revoked_at IS NULL`, userID, at)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) RevokeAllSessionsForUserTx(ctx context.Context, tx pgx.Tx, userID string, at time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at = $2 WHERE admin_user_id = $1 AND revoked_at IS NULL`, userID, at)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) InsertAuditEvent(ctx context.Context, adminUserID *string, action string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	var id any
	if adminUserID != nil && *adminUserID != "" {
		id = *adminUserID
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO admin_audit_events (admin_user_id, action, metadata) VALUES ($1, $2, $3::jsonb)`, id, action, payload)
	return err
}

func (r *Repository) InsertAuditEventTx(ctx context.Context, tx pgx.Tx, adminUserID *string, action string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	var id any
	if adminUserID != nil && *adminUserID != "" {
		id = *adminUserID
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit_events (admin_user_id, action, metadata) VALUES ($1, $2, $3::jsonb)`, id, action, payload)
	return err
}

type scannable interface {
	Scan(dest ...any) error
}

func scanUser(row scannable) (User, error) {
	var u User
	var role string
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &role, &u.IsActive, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Role = Role(role)
	return u, nil
}

func scanSession(row scannable) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.AdminUserID, &s.TokenHash, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt, &s.LastSeenAt, &s.CreatedIP, &s.UserAgent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, err
	}
	return s, nil
}
