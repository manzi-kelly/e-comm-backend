package customer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidCredentials = errors.New("invalid customer credentials")
	ErrDuplicateCustomer  = errors.New("customer already exists")
	ErrRateLimited        = errors.New("customer request rate limited")
	ErrSessionInvalid     = errors.New("customer session invalid")
)

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

func timingHash() string {
	dummyHashOnce.Do(func() {
		hash, err := auth.HashPassword("Timing-Customer-Password-123!")
		if err == nil {
			dummyHash = hash
		}
	})
	return dummyHash
}

// Service implements customer registration and private session management.
// It shares password/token primitives with admin auth but uses its own limiter,
// repository, tables and principal type.
type Service struct {
	repo    *Repository
	pool    *pgxpool.Pool
	cfg     config.Config
	limiter *auth.LoginLimiter
	logger  *slog.Logger
	now     func() time.Time
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

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func truncateMeta(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 512 {
		return v[:512]
	}
	return v
}

func metaPointers(meta LoginMeta) (*string, *string) {
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

// Register validates server-normalized input, hashes the password with
// Argon2id and atomically creates the account, first session and audit event.
func (s *Service) Register(ctx context.Context, in NormalizedRegistration, meta LoginMeta) (Customer, Session, string, string, error) {
	ipKey := "customer-register:ip:" + meta.IP
	if ok, _ := s.limiter.Allow(ipKey); !ok {
		return Customer{}, Session{}, "", "", ErrRateLimited
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("hash customer password: %w", err)
	}
	sessionToken, err := auth.GenerateToken(32)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("generate customer session token: %w", err)
	}
	csrfToken, err := auth.GenerateToken(32)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("generate customer csrf token: %w", err)
	}
	now := s.now()
	expiresAt := now.Add(s.cfg.CustomerAuth.SessionTTL)
	ip, ua := metaPointers(meta)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("begin registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	account, err := s.repo.CreateTx(ctx, tx, in, hash)
	if err != nil {
		if isUniqueViolation(err) {
			s.limiter.RecordFailure(ipKey)
			return Customer{}, Session{}, "", "", ErrDuplicateCustomer
		}
		return Customer{}, Session{}, "", "", fmt.Errorf("create customer: %w", err)
	}
	sess, err := s.repo.CreateSessionTx(ctx, tx, account.ID, auth.HashToken(sessionToken), expiresAt, ip, ua)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("create customer session: %w", err)
	}
	cid := account.ID
	if err := s.repo.InsertAuditEventTx(ctx, tx, &cid, "customer.registered", map[string]any{"session_id": sess.ID}); err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("record registration audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return Customer{}, Session{}, "", "", ErrDuplicateCustomer
		}
		return Customer{}, Session{}, "", "", fmt.Errorf("commit registration: %w", err)
	}
	account.LastLoginAt = &now
	s.limiter.RecordSuccess(ipKey)
	return account, sess, sessionToken, csrfToken, nil
}

// Login authenticates an account without revealing whether an email exists.
func (s *Service) Login(ctx context.Context, email, password string, meta LoginMeta) (Customer, Session, string, string, error) {
	email = NormalizeEmail(email)
	if email == "" || password == "" || len(password) > 4096 || len(email) > 320 {
		return Customer{}, Session{}, "", "", ErrInvalidCredentials
	}
	ipKey := "customer-login:ip:" + meta.IP
	emailKey := "customer-login:acct:" + email
	for _, key := range []string{ipKey, emailKey} {
		if ok, _ := s.limiter.Allow(key); !ok {
			s.audit(ctx, nil, "customer.login.rate_limited", map[string]any{"scope": map[bool]string{true: "ip", false: "account"}[key == ipKey]})
			return Customer{}, Session{}, "", "", ErrRateLimited
		}
	}

	account, err := s.repo.FindByEmail(ctx, email)
	if err != nil {
		_ = auth.VerifyPassword(password, timingHash())
		s.limiter.RecordFailure(ipKey)
		s.limiter.RecordFailure(emailKey)
		s.audit(ctx, nil, "customer.login.failed", map[string]any{"reason": "invalid_credentials"})
		return Customer{}, Session{}, "", "", ErrInvalidCredentials
	}
	// Disabled accounts still run password verification but never receive a
	// distinct client-visible failure reason.
	if err := auth.VerifyPassword(password, account.PasswordHash); err != nil || account.Status != StatusActive {
		s.limiter.RecordFailure(ipKey)
		s.limiter.RecordFailure(emailKey)
		cid := account.ID
		reason := "invalid_credentials"
		if err == nil {
			reason = "account_disabled"
		}
		s.audit(ctx, &cid, "customer.login.failed", map[string]any{"reason": reason})
		return Customer{}, Session{}, "", "", ErrInvalidCredentials
	}

	sessionToken, err := auth.GenerateToken(32)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("generate customer session token: %w", err)
	}
	csrfToken, err := auth.GenerateToken(32)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("generate customer csrf token: %w", err)
	}
	now := s.now()
	expiresAt := now.Add(s.cfg.CustomerAuth.SessionTTL)
	ip, ua := metaPointers(meta)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("begin customer login: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sess, err := s.repo.CreateSessionTx(ctx, tx, account.ID, auth.HashToken(sessionToken), expiresAt, ip, ua)
	if err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("create customer session: %w", err)
	}
	if err := s.repo.UpdateLastLoginTx(ctx, tx, account.ID, now); err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("update customer last login: %w", err)
	}
	cid := account.ID
	if err := s.repo.InsertAuditEventTx(ctx, tx, &cid, "customer.login.success", map[string]any{"session_id": sess.ID}); err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("record customer login audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Customer{}, Session{}, "", "", fmt.Errorf("commit customer login: %w", err)
	}
	account.LastLoginAt = &now
	s.limiter.RecordSuccess(ipKey)
	s.limiter.RecordSuccess(emailKey)
	return account, sess, sessionToken, csrfToken, nil
}

// Authenticate resolves a raw cookie token to a valid server-side session.
func (s *Service) Authenticate(ctx context.Context, sessionToken string) (Customer, Session, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return Customer{}, Session{}, ErrSessionInvalid
	}
	sess, account, err := s.repo.FindActiveSession(ctx, auth.HashToken(sessionToken))
	if err != nil {
		return Customer{}, Session{}, ErrSessionInvalid
	}
	_ = s.repo.TouchSession(ctx, sess.ID, s.now())
	return account, sess, nil
}

// Logout revokes exactly one customer session and audits the event atomically.
func (s *Service) Logout(ctx context.Context, account Customer, sess Session) error {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin customer logout: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.repo.RevokeSessionTx(ctx, tx, sess.ID, now); err != nil {
		return fmt.Errorf("revoke customer session: %w", err)
	}
	cid := account.ID
	if err := s.repo.InsertAuditEventTx(ctx, tx, &cid, "customer.logout", map[string]any{"session_id": sess.ID}); err != nil {
		return fmt.Errorf("record customer logout audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit customer logout: %w", err)
	}
	return nil
}

func (s *Service) audit(ctx context.Context, customerID *string, action string, metadata map[string]any) {
	if err := s.repo.InsertAuditEvent(ctx, customerID, action, metadata); err != nil {
		s.logger.Warn("customer audit event insert failed", "action", action)
	}
}

// SetNow and SetLimiter are test hooks; production callers never use them.
func (s *Service) SetNow(now func() time.Time)           { s.now = now }
func (s *Service) SetLimiter(limiter *auth.LoginLimiter) { s.limiter = limiter }
