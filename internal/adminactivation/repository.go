package adminactivation

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// activationLockID serializes provisioning, code issuance, verification and
// completion. All activation writes take this transaction-scoped advisory lock
// before touching activation or administrator tables.
const activationLockID int64 = 0x4155544956414331

// Repository owns PostgreSQL access for activation state. Every mutating
// method takes an explicit transaction so callers control atomicity.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const authorizationColumns = `
	id, email, display_name, role, status, activation_token_hash,
	activation_expires_at, activated_user_id, created_at`

func (r *Repository) LockTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, activationLockID)
	return err
}

// LockAdminUsersTx serializes activation against the legacy CLI bootstrap
// empty-table check. It must always be taken after LockTx.
func (r *Repository) LockAdminUsersTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE admin_users IN EXCLUSIVE MODE`)
	return err
}

func (r *Repository) CountAdminsTx(ctx context.Context, tx pgx.Tx) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n)
	return n, err
}

func (r *Repository) FindAuthorization(ctx context.Context, q queryRower, email string) (Authorization, error) {
	const qry = `SELECT ` + authorizationColumns + `
		FROM admin_activation_authorizations WHERE lower(email) = $1`
	return scanAuthorization(q.QueryRow(ctx, qry, strings.ToLower(strings.TrimSpace(email))))
}

// FindAuthorizationForUpdate locks the single authorization row for the
// duration of a verification or completion transaction.
func (r *Repository) FindAuthorizationForUpdate(ctx context.Context, tx pgx.Tx, email string) (Authorization, error) {
	const qry = `SELECT ` + authorizationColumns + `
		FROM admin_activation_authorizations WHERE lower(email) = $1 FOR UPDATE`
	return scanAuthorization(tx.QueryRow(ctx, qry, strings.ToLower(strings.TrimSpace(email))))
}

func (r *Repository) InsertAuthorizationTx(ctx context.Context, tx pgx.Tx, email, displayName string, now time.Time) (Authorization, error) {
	const qry = `
		INSERT INTO admin_activation_authorizations (email, display_name, role, status, created_at, updated_at)
		VALUES (lower($1), btrim($2), $3, 'pending', $4, $4)
		RETURNING ` + authorizationColumns
	return scanAuthorization(tx.QueryRow(ctx, qry, email, displayName, InitialRole, now))
}

// CountRecentCodesTx counts all codes issued to an authorization since the
// supplied instant. It backs resend caps and request cooldowns.
func (r *Repository) CountRecentCodesTx(ctx context.Context, tx pgx.Tx, authorizationID string, since time.Time) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM admin_activation_codes
		WHERE authorization_id = $1 AND created_at >= $2`, authorizationID, since).Scan(&n)
	return n, err
}

// OldestCodeSinceTx returns the earliest code creation time in the window. It
// is used only to compute an accurate Retry-After for resend-cap throttling.
// A zero time means no matching row exists.
func (r *Repository) OldestCodeSinceTx(ctx context.Context, tx pgx.Tx, authorizationID string, since time.Time) (time.Time, error) {
	return r.scanCodeTime(ctx, tx, `
		SELECT created_at FROM admin_activation_codes
		WHERE authorization_id = $1 AND created_at >= $2
		ORDER BY created_at ASC, id ASC LIMIT 1`, authorizationID, since)
}

// NewestCodeAtOrBeforeTx returns the most recent code creation time, used to
// compute the remaining per-request cooldown without exposing any code data.
func (r *Repository) NewestCodeAtOrBeforeTx(ctx context.Context, tx pgx.Tx, authorizationID string, until time.Time) (time.Time, error) {
	return r.scanCodeTime(ctx, tx, `
		SELECT created_at FROM admin_activation_codes
		WHERE authorization_id = $1 AND created_at <= $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, authorizationID, until)
}

func (r *Repository) scanCodeTime(ctx context.Context, tx pgx.Tx, query string, args ...any) (time.Time, error) {
	var created pgtype.Timestamptz
	err := tx.QueryRow(ctx, query, args...).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if !created.Valid {
		return time.Time{}, nil
	}
	return created.Time, nil
}

// CountRecentRequestsByIPAndEmailTx provides IP/email throttling from durable
// state so throttling survives restarts and applies across server instances.
func (r *Repository) CountRecentRequestsByIPAndEmailTx(ctx context.Context, tx pgx.Tx, email, ip string, since time.Time) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM admin_activation_codes c
		JOIN admin_activation_authorizations a ON a.id = c.authorization_id
		WHERE lower(a.email) = lower($1) AND c.created_ip = $2 AND c.created_at >= $3`,
		email, ip, since).Scan(&n)
	return n, err
}

// OldestRecentRequestByIPAndEmailTx returns the earliest durable request in the
// IP/email window so its Retry-After can reflect the real window expiry.
func (r *Repository) OldestRecentRequestByIPAndEmailTx(ctx context.Context, tx pgx.Tx, email, ip string, since time.Time) (time.Time, error) {
	var created pgtype.Timestamptz
	err := tx.QueryRow(ctx, `
		SELECT c.created_at FROM admin_activation_codes c
		JOIN admin_activation_authorizations a ON a.id = c.authorization_id
		WHERE lower(a.email) = lower($1) AND c.created_ip = $2 AND c.created_at >= $3
		ORDER BY c.created_at ASC, c.id ASC LIMIT 1`, email, ip, since).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if !created.Valid {
		return time.Time{}, nil
	}
	return created.Time, nil
}

// InvalidateCodesTx consumes every still-live code, so only one code can be
// usable at a time.
func (r *Repository) InvalidateCodesTx(ctx context.Context, tx pgx.Tx, authorizationID string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE admin_activation_codes SET consumed_at = $2
		WHERE authorization_id = $1 AND consumed_at IS NULL`, authorizationID, now)
	return err
}

func (r *Repository) InsertCodeTx(ctx context.Context, tx pgx.Tx, authorizationID string, codeHash []byte, expiresAt, now time.Time, ip string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO admin_activation_codes
			(authorization_id, code_hash, expires_at, failed_attempts, consumed_at, created_at, created_ip)
		VALUES ($1, $2, $3, 0, NULL, $4, NULLIF($5, ''))`,
		authorizationID, codeHash, expiresAt, now, ip)
	return err
}

// FindUsableCodeForUpdate returns only the newest unconsumed code and locks it.
// A concurrently verified code is therefore never accepted twice.
func (r *Repository) FindUsableCodeForUpdate(ctx context.Context, tx pgx.Tx, authorizationID string) (ActivationCode, error) {
	const qry = `
		SELECT id, authorization_id, code_hash, expires_at, failed_attempts, created_at
		FROM admin_activation_codes
		WHERE authorization_id = $1 AND consumed_at IS NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1 FOR UPDATE`
	row := tx.QueryRow(ctx, qry, authorizationID)
	var c ActivationCode
	err := row.Scan(&c.ID, &c.AuthorizationID, &c.CodeHash, &c.ExpiresAt, &c.FailedAttempts, &c.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ActivationCode{}, ErrInvalidCode
		}
		return ActivationCode{}, err
	}
	return c, nil
}

func (r *Repository) RecordFailedAttemptTx(ctx context.Context, tx pgx.Tx, codeID string, now time.Time, exhausted bool) error {
	if exhausted {
		_, err := tx.Exec(ctx, `
			UPDATE admin_activation_codes
			SET failed_attempts = failed_attempts + 1, consumed_at = $2
			WHERE id = $1 AND consumed_at IS NULL`, codeID, now)
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE admin_activation_codes SET failed_attempts = failed_attempts + 1
		WHERE id = $1 AND consumed_at IS NULL`, codeID)
	return err
}

func (r *Repository) SetActivationTokenTx(ctx context.Context, tx pgx.Tx, authorizationID string, tokenHash []byte, expiresAt, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE admin_activation_authorizations
		SET activation_token_hash = $2, activation_expires_at = $3, updated_at = $4
		WHERE id = $1 AND status = 'pending'`, authorizationID, tokenHash, expiresAt, now)
	return err
}

func (r *Repository) MarkActivatedTx(ctx context.Context, tx pgx.Tx, authorizationID, userID string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE admin_activation_authorizations
		SET status = 'activated', activation_token_hash = NULL, activation_expires_at = NULL,
		    activated_user_id = $2, activated_at = $3, updated_at = $3
		WHERE id = $1 AND status = 'pending'`, authorizationID, userID, now)
	return err
}

func scanAuthorization(row pgx.Row) (Authorization, error) {
	var a Authorization
	var role, status string
	var token []byte
	var expires pgtype.Timestamptz
	var userID pgtype.Text
	err := row.Scan(&a.ID, &a.Email, &a.DisplayName, &role, &status, &token, &expires, &userID, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Authorization{}, ErrNotFound
		}
		return Authorization{}, err
	}
	a.Role = role
	a.Status = status
	a.ActivationTokenHash = token
	if expires.Valid {
		a.ActivationExpiresAt = &expires.Time
	}
	if userID.Valid {
		a.ActivatedUserID = &userID.String
	}
	return a, nil
}

// ErrNotFound is returned when no authorization row exists.
var ErrNotFound = errors.New("activation authorization not found")
