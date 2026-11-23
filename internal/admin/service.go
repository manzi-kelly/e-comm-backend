package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAccountDisabled    = errors.New("account disabled")
	ErrRateLimited        = errors.New("rate limited")
	ErrSessionInvalid     = errors.New("session invalid")
)

// Service implements administrator authentication and session management.
type Service struct {
	repo    *Repository
	pool    *pgxpool.Pool
	cfg     config.Config
	limiter *auth.LoginLimiter
	logger  *slog.Logger
	now     func() time.Time
}

type LoginMeta struct {
	IP        string
	UserAgent string
}

func NewService(pool *pgxpool.Pool, cfg config.Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		repo:    NewRepository(pool),
		pool:    pool,
		cfg:     cfg,
		limiter: auth.NewLoginLimiter(cfg.Auth.LoginMaxFailures, cfg.Auth.LoginLockoutWindow),
		logger:  logger,
		now:     time.Now,
	}
}

func (s *Service) Limiter() *auth.LoginLimiter { return s.limiter }
func (s *Service) Repository() *Repository     { return s.repo }

// Login authenticates an administrator, rotates session identity, and persists session/audit state atomically.
func (s *Service) Login(ctx context.Context, email, password string, meta LoginMeta) (User, Session, string, string, error) {
	email = NormalizeEmail(email)
	if email == "" || password == "" || len(password) > 4096 {
		return User{}, Session{}, "", "", ErrInvalidCredentials
	}

	ipKey := "ip:" + meta.IP
	emailKey := "acct:" + email
	if ok, _ := s.limiter.Allow(ipKey); !ok {
		s.audit(ctx, nil, "admin.login.rate_limited", map[string]any{"scope": "ip"})
		return User{}, Session{}, "", "", ErrRateLimited
	}
	if ok, _ := s.limiter.Allow(emailKey); !ok {
		s.audit(ctx, nil, "admin.login.rate_limited", map[string]any{"scope": "account"})
		return User{}, Session{}, "", "", ErrRateLimited
	}

	user, err := s.repo.FindUserByEmail(ctx, email)
	if err != nil {
		// Perform a dummy verification to reduce timing differences.
		_, _ = auth.HashPassword("dummy-password-for-timing-0000")
		s.limiter.RecordFailure(ipKey)
		s.limiter.RecordFailure(emailKey)
		s.audit(ctx, nil, "admin.login.failed", map[string]any{"reason": "invalid_credentials"})
		return User{}, Session{}, "", "", ErrInvalidCredentials
	}
	if !user.IsActive {
		_ = auth.VerifyPassword(password, user.PasswordHash)
		s.limiter.RecordFailure(ipKey)
		s.limiter.RecordFailure(emailKey)
		uid := user.ID
		s.audit(ctx, &uid, "admin.login.failed", map[string]any{"reason": "account_disabled"})
		return User{}, Session{}, "", "", ErrAccountDisabled
	}
	if err := auth.VerifyPassword(password, user.PasswordHash); err != nil {
		s.limiter.RecordFailure(ipKey)
		s.limiter.RecordFailure(emailKey)
		uid := user.ID
		s.audit(ctx, &uid, "admin.login.failed", map[string]any{"reason": "invalid_credentials"})
		return User{}, Session{}, "", "", ErrInvalidCredentials
	}

	sessionToken, err := auth.GenerateToken(32)
	if err != nil {
		return User{}, Session{}, "", "", fmt.Errorf("generate session token: %w", err)
	}
	csrfToken, err := auth.GenerateToken(32)
	if err != nil {
		return User{}, Session{}, "", "", fmt.Errorf("generate csrf token: %w", err)
	}
	now := s.now()
	expiresAt := now.Add(s.cfg.Auth.SessionTTL)
	tokenHash := auth.HashToken(sessionToken)
	ip := truncateMeta(meta.IP)
	ua := truncateMeta(meta.UserAgent)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, Session{}, "", "", fmt.Errorf("begin login transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sess, err := s.repo.CreateSessionTx(ctx, tx, user.ID, tokenHash, expiresAt, ptrIfNotEmpty(ip), ptrIfNotEmpty(ua))
	if err != nil {
		return User{}, Session{}, "", "", fmt.Errorf("create session: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_users SET last_login_at = $2, updated_at = now() WHERE id = $1`, user.ID, now); err != nil {
		return User{}, Session{}, "", "", fmt.Errorf("update last login: %w", err)
	}
	uid := user.ID
	if err := s.repo.InsertAuditEventTx(ctx, tx, &uid, "admin.login.success", map[string]any{"session_id": sess.ID}); err != nil {
		return User{}, Session{}, "", "", fmt.Errorf("record login audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, Session{}, "", "", fmt.Errorf("commit login: %w", err)
	}

	// Refresh user view for response (last login updated).
	user.LastLoginAt = &now
	user.UpdatedAt = now
	s.limiter.RecordSuccess(ipKey)
	s.limiter.RecordSuccess(emailKey)
	return user, sess, sessionToken, csrfToken, nil
}

// Authenticate resolves and validates a session token from a cookie.
func (s *Service) Authenticate(ctx context.Context, sessionToken string) (User, Session, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return User{}, Session{}, ErrSessionInvalid
	}
	sess, user, err := s.repo.FindActiveSession(ctx, auth.HashToken(sessionToken))
	if err != nil {
		return User{}, Session{}, ErrSessionInvalid
	}
	// Best-effort last seen touch; ignore errors.
	_ = s.repo.TouchSession(ctx, sess.ID, s.now())
	return user, sess, nil
}

// Logout revokes the current session.
func (s *Service) Logout(ctx context.Context, user User, session Session) error {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin logout transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, session.ID, now); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	uid := user.ID
	if err := s.repo.InsertAuditEventTx(ctx, tx, &uid, "admin.logout", map[string]any{"session_id": session.ID}); err != nil {
		return fmt.Errorf("record logout audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit logout: %w", err)
	}
	return nil
}

// LogoutAll revokes all sessions for the authenticated administrator.
func (s *Service) LogoutAll(ctx context.Context, user User) (int64, error) {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin logout-all transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	count, err := s.repo.RevokeAllSessionsForUserTx(ctx, tx, user.ID, now)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	uid := user.ID
	if err := s.repo.InsertAuditEventTx(ctx, tx, &uid, "admin.logout_all", map[string]any{"revoked_count": count}); err != nil {
		return 0, fmt.Errorf("record logout-all audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit logout-all: %w", err)
	}
	return count, nil
}

func (s *Service) audit(ctx context.Context, userID *string, action string, metadata map[string]any) {
	if err := s.repo.InsertAuditEvent(ctx, userID, action, metadata); err != nil {
		s.logger.Warn("audit event insert failed", "action", action)
	}
}

func truncateMeta(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 512 {
		return v[:512]
	}
	return v
}

func ptrIfNotEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// For tests: override time and limiter behavior hooks.
func (s *Service) SetNow(now func() time.Time)     { s.now = now }
func (s *Service) SetLimiter(l *auth.LoginLimiter) { s.limiter = l }
