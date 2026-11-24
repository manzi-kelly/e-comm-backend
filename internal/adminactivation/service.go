package adminactivation

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Service coordinates provisioning, code issuance, verification and the
// single-use authorization that permits password creation.
type Service struct {
	pool            *pgxpool.Pool
	repo            *Repository
	adminRepo       *admin.Repository
	cfg             config.AdminActivationConfig
	sender          EmailSender
	logger          *slog.Logger
	now             func() time.Time
	requestLimiter  *auth.LoginLimiter
	verifyLimiter   *auth.LoginLimiter
	completeLimiter *auth.LoginLimiter
}

func NewService(pool *pgxpool.Pool, cfg config.AdminActivationConfig, sender EmailSender, logger *slog.Logger) (*Service, error) {
	if pool == nil || !cfg.Enabled || sender == nil {
		return nil, ErrUnconfigured
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		pool:            pool,
		repo:            NewRepository(pool),
		adminRepo:       admin.NewRepository(pool),
		cfg:             cfg,
		sender:          sender,
		logger:          logger,
		now:             time.Now,
		requestLimiter:  auth.NewLoginLimiter(cfg.MaxResends, cfg.ResendWindow),
		verifyLimiter:   auth.NewLoginLimiter(cfg.MaxVerifyAttempts, cfg.ResendWindow),
		completeLimiter: auth.NewLoginLimiter(cfg.MaxVerifyAttempts, cfg.ResendWindow),
	}, nil
}

// NewProvisioner builds the narrow subset of Service used by the explicit
// development-database setup command. It does not require SMTP configuration
// and exposes no code issuance or password creation capability.
func NewProvisioner(pool *pgxpool.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		pool:            pool,
		repo:            NewRepository(pool),
		adminRepo:       admin.NewRepository(pool),
		logger:          logger,
		now:             time.Now,
		requestLimiter:  auth.NewLoginLimiter(3, time.Hour),
		verifyLimiter:   auth.NewLoginLimiter(5, time.Hour),
		completeLimiter: auth.NewLoginLimiter(5, time.Hour),
	}
}

func (s *Service) Repository() *Repository { return s.repo }

// SetNow is a deterministic test hook that also controls limiter windows.
func (s *Service) SetNow(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	s.now = now
	s.requestLimiter.SetNow(now)
	s.verifyLimiter.SetNow(now)
	s.completeLimiter.SetNow(now)
}

// Provision explicitly authorizes the single pre-authorized email address. It
// creates no administrator account and is idempotent. It refuses to run when
// any administrator already exists, so existing accounts are never overwritten.
func (s *Service) Provision(ctx context.Context, displayName string) (bool, error) {
	displayName = strings.TrimSpace(displayName)
	if len(displayName) < 1 || len(displayName) > 120 {
		return false, fmt.Errorf("display name must be between 1 and 120 characters")
	}
	email := config.AuthorizedAdminEmail

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin provisioning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.repo.LockTx(ctx, tx); err != nil {
		return false, fmt.Errorf("lock activation state: %w", err)
	}
	if err := s.repo.LockAdminUsersTx(ctx, tx); err != nil {
		return false, fmt.Errorf("lock administrator table: %w", err)
	}
	count, err := s.repo.CountAdminsTx(ctx, tx)
	if err != nil {
		return false, fmt.Errorf("inspect administrators: %w", err)
	}
	if count > 0 {
		return false, ErrAdminExists
	}
	authz, err := s.repo.FindAuthorization(ctx, tx, email)
	if err == nil {
		if authz.Status == "activated" {
			return false, ErrAlreadyActivated
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit provisioning: %w", err)
		}
		return false, nil // already provisioned; no state change
	}
	if !errors.Is(err, ErrNotFound) {
		return false, fmt.Errorf("inspect activation authorization: %w", err)
	}

	if _, err := s.repo.InsertAuthorizationTx(ctx, tx, email, displayName, s.now()); err != nil {
		return false, fmt.Errorf("insert activation authorization: %w", err)
	}
	if err := s.adminRepo.InsertAuditEventTx(ctx, tx, nil, "admin.activation.provisioned",
		map[string]any{"role": InitialRole}); err != nil {
		return false, fmt.Errorf("record provisioning audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit provisioning: %w", err)
	}
	return true, nil
}

// RequestCode issues a code only for the provisioned authorized email. Unknown
// emails produce the same observable result and never trigger email delivery.
func (s *Service) RequestCode(ctx context.Context, email, ip string) error {
	email = admin.NormalizeEmail(email)
	ip = truncateMeta(ip)
	if !validEmail(email) {
		// Equalize work for unauthorized input without revealing it.
		_ = HashCode(s.cfg.OTPKey, email, "000000")
		return nil
	}

	ipKey := "activation-request-ip:" + ip
	emailKey := "activation-request-email:" + email
	if ok, retry := s.requestLimiter.Allow(ipKey); !ok {
		return newRateLimitError(retry)
	}
	if ok, retry := s.requestLimiter.Allow(emailKey); !ok {
		return newRateLimitError(retry)
	}
	// Every request counts toward the throttle, including requests for an
	// unauthorized address, so throttling cannot be used to probe accounts.
	s.requestLimiter.RecordFailure(ipKey)
	s.requestLimiter.RecordFailure(emailKey)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin code issuance transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.repo.LockTx(ctx, tx); err != nil {
		return fmt.Errorf("lock activation state: %w", err)
	}

	authz, err := s.repo.FindAuthorizationForUpdate(ctx, tx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = HashCode(s.cfg.OTPKey, email, "000000")
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit no-op code request: %w", err)
			}
			return nil
		}
		return fmt.Errorf("find activation authorization: %w", err)
	}
	if authz.Status != "pending" {
		_ = HashCode(s.cfg.OTPKey, email, "000000")
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit no-op code request: %w", err)
		}
		return nil
	}

	now := s.now()
	recentSince := now.Add(-s.cfg.ResendWindow)
	recent, err := s.repo.CountRecentCodesTx(ctx, tx, authz.ID, recentSince)
	if err != nil {
		return fmt.Errorf("count recent codes: %w", err)
	}
	if recent >= int64(s.cfg.MaxResends) {
		oldest, oldestErr := s.repo.OldestCodeSinceTx(ctx, tx, authz.ID, recentSince)
		if oldestErr != nil {
			return fmt.Errorf("inspect recent code window: %w", oldestErr)
		}
		return newRateLimitError(remainingWait(oldest, s.cfg.ResendWindow, now))
	}
	cooldownSince := now.Add(-s.cfg.RequestCooldown)
	coolDown, err := s.repo.CountRecentCodesTx(ctx, tx, authz.ID, cooldownSince)
	if err != nil {
		return fmt.Errorf("count cooldown codes: %w", err)
	}
	if coolDown > 0 {
		newest, newestErr := s.repo.NewestCodeAtOrBeforeTx(ctx, tx, authz.ID, now)
		if newestErr != nil {
			return fmt.Errorf("inspect request cooldown: %w", newestErr)
		}
		return newRateLimitError(remainingWait(newest, s.cfg.RequestCooldown, now))
	}
	if ip != "" {
		byIP, ipErr := s.repo.CountRecentRequestsByIPAndEmailTx(ctx, tx, authz.Email, ip, recentSince)
		if ipErr != nil {
			return fmt.Errorf("count recent requests by IP: %w", ipErr)
		}
		if byIP >= int64(s.cfg.MaxResends) {
			oldest, oldestErr := s.repo.OldestRecentRequestByIPAndEmailTx(ctx, tx, authz.Email, ip, recentSince)
			if oldestErr != nil {
				return fmt.Errorf("inspect IP request window: %w", oldestErr)
			}
			return newRateLimitError(remainingWait(oldest, s.cfg.ResendWindow, now))
		}
	}

	code, err := GenerateCode()
	if err != nil {
		return fmt.Errorf("generate code: %w", err)
	}
	codeHash := HashCode(s.cfg.OTPKey, authz.Email, code)
	if err := s.repo.InvalidateCodesTx(ctx, tx, authz.ID, now); err != nil {
		return fmt.Errorf("invalidate older codes: %w", err)
	}
	if err := s.repo.InsertCodeTx(ctx, tx, authz.ID, codeHash, now.Add(ActivationCodeTTL), now, ip); err != nil {
		return fmt.Errorf("store activation code: %w", err)
	}
	if err := s.adminRepo.InsertAuditEventTx(ctx, tx, nil, "admin.activation.code_issued",
		map[string]any{"authorization_id": authz.ID}); err != nil {
		return fmt.Errorf("record code issuance audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit code issuance: %w", err)
	}

	// Delivery happens after commit so a network failure cannot leave a
	// supposedly successful activation attempt behind. A failed delivery
	// invalidates the code durably and is reported truthfully.
	if err := s.sender.SendActivationCode(ctx, authz.Email, code); err != nil {
		s.invalidateAfterDeliveryFailure(ctx, authz.ID)
		return ErrEmailDelivery
	}
	return nil
}

func (s *Service) invalidateAfterDeliveryFailure(ctx context.Context, authorizationID string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logger.Warn("activation delivery failure cleanup could not begin")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.repo.LockTx(ctx, tx); err != nil {
		return
	}
	if err := s.repo.InvalidateCodesTx(ctx, tx, authorizationID, s.now()); err != nil {
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.logger.Warn("activation delivery failure cleanup could not commit")
	}
}

// VerifyCode atomically consumes a valid code and issues a short-lived,
// single-use authorization token for password creation.
func (s *Service) VerifyCode(ctx context.Context, email, code, ip string) (string, error) {
	email = admin.NormalizeEmail(email)
	ip = truncateMeta(ip)
	ipKey := "activation-verify-ip:" + ip
	emailKey := "activation-verify-email:" + email
	if ok, retry := s.verifyLimiter.Allow(ipKey); !ok {
		return "", newRateLimitError(retry)
	}
	if ok, retry := s.verifyLimiter.Allow(emailKey); !ok {
		return "", newRateLimitError(retry)
	}

	if !validEmail(email) || !ValidCodeFormat(code) {
		s.verifyLimiter.RecordFailure(ipKey)
		s.verifyLimiter.RecordFailure(emailKey)
		return "", ErrInvalidCode
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin verification transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.repo.LockTx(ctx, tx); err != nil {
		return "", fmt.Errorf("lock activation state: %w", err)
	}
	authz, err := s.repo.FindAuthorizationForUpdate(ctx, tx, email)
	if err != nil {
		s.verifyLimiter.RecordFailure(ipKey)
		s.verifyLimiter.RecordFailure(emailKey)
		if errors.Is(err, ErrNotFound) {
			_ = HashCode(s.cfg.OTPKey, email, code)
			return "", ErrInvalidCode
		}
		return "", fmt.Errorf("find activation authorization: %w", err)
	}

	codeRow, err := s.repo.FindUsableCodeForUpdate(ctx, tx, authz.ID)
	if err != nil {
		s.verifyLimiter.RecordFailure(ipKey)
		s.verifyLimiter.RecordFailure(emailKey)
		if errors.Is(err, ErrInvalidCode) {
			return "", ErrInvalidCode
		}
		return "", fmt.Errorf("find activation code: %w", err)
	}

	now := s.now()
	if !now.Before(codeRow.ExpiresAt) {
		if err := s.repo.InvalidateCodesTx(ctx, tx, authz.ID, now); err != nil {
			return "", fmt.Errorf("invalidate expired code: %w", err)
		}
		if err := s.adminRepo.InsertAuditEventTx(ctx, tx, nil, "admin.activation.code_expired",
			map[string]any{"authorization_id": authz.ID}); err != nil {
			return "", fmt.Errorf("record expired code audit: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit expired code: %w", err)
		}
		s.verifyLimiter.RecordFailure(ipKey)
		s.verifyLimiter.RecordFailure(emailKey)
		return "", ErrInvalidCode
	}

	if authz.Status != "pending" || !CodeMatches(s.cfg.OTPKey, authz.Email, code, codeRow.CodeHash) {
		exhausted := codeRow.FailedAttempts+1 >= s.cfg.MaxVerifyAttempts
		if err := s.repo.RecordFailedAttemptTx(ctx, tx, codeRow.ID, now, exhausted); err != nil {
			return "", fmt.Errorf("record failed verification: %w", err)
		}
		if err := s.adminRepo.InsertAuditEventTx(ctx, tx, nil, "admin.activation.code_rejected",
			map[string]any{"authorization_id": authz.ID, "exhausted": exhausted}); err != nil {
			return "", fmt.Errorf("record failed verification audit: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit failed verification: %w", err)
		}
		s.verifyLimiter.RecordFailure(ipKey)
		s.verifyLimiter.RecordFailure(emailKey)
		return "", ErrInvalidCode
	}

	token, err := auth.GenerateToken(32)
	if err != nil {
		return "", fmt.Errorf("generate activation authorization: %w", err)
	}
	if err := s.repo.InvalidateCodesTx(ctx, tx, authz.ID, now); err != nil {
		return "", fmt.Errorf("consume activation code: %w", err)
	}
	if err := s.repo.SetActivationTokenTx(ctx, tx, authz.ID, auth.HashToken(token), now.Add(s.cfg.AuthorizationTTL), now); err != nil {
		return "", fmt.Errorf("store activation authorization: %w", err)
	}
	if err := s.adminRepo.InsertAuditEventTx(ctx, tx, nil, "admin.activation.code_verified",
		map[string]any{"authorization_id": authz.ID}); err != nil {
		return "", fmt.Errorf("record verification audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit verification: %w", err)
	}
	s.verifyLimiter.RecordSuccess(ipKey)
	s.verifyLimiter.RecordSuccess(emailKey)
	return token, nil
}

// Complete creates the super_admin account only after a valid, unexpired,
// single-use activation token and a strong password are supplied.
func (s *Service) Complete(ctx context.Context, email, token, password, ip string) (user admin.User, err error) {
	email = admin.NormalizeEmail(email)
	ip = truncateMeta(ip)
	ipKey := "activation-complete-ip:" + ip
	emailKey := "activation-complete-email:" + email
	if ok, retry := s.completeLimiter.Allow(ipKey); !ok {
		return admin.User{}, newRateLimitError(retry)
	}
	if ok, retry := s.completeLimiter.Allow(emailKey); !ok {
		return admin.User{}, newRateLimitError(retry)
	}
	defer func() {
		if err != nil {
			s.completeLimiter.RecordFailure(ipKey)
			s.completeLimiter.RecordFailure(emailKey)
		} else {
			s.completeLimiter.RecordSuccess(ipKey)
			s.completeLimiter.RecordSuccess(emailKey)
		}
	}()

	if err := auth.ValidatePasswordStrength(password); err != nil {
		return admin.User{}, ErrPasswordWeak
	}
	if strings.TrimSpace(token) == "" || len(token) > 4096 {
		return admin.User{}, ErrInvalidToken
	}

	// Cheap read-only check first: Argon2id is expensive and should not be
	// performed for random tokens. The authoritative check is repeated inside
	// the locked transaction below.
	pre, preErr := s.repo.FindAuthorization(ctx, s.pool, email)
	if preErr != nil || pre.Status != "pending" || pre.ActivationExpiresAt == nil ||
		!s.now().Before(*pre.ActivationExpiresAt) || len(pre.ActivationTokenHash) == 0 ||
		subtle.ConstantTimeCompare(pre.ActivationTokenHash, auth.HashToken(token)) != 1 {
		return admin.User{}, ErrInvalidToken
	}

	// Argon2id is expensive; compute it before taking database locks.
	hash, err := auth.HashPassword(password)
	if err != nil {
		return admin.User{}, ErrPasswordWeak
	}
	tokenHash := auth.HashToken(token)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return admin.User{}, fmt.Errorf("begin activation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.repo.LockTx(ctx, tx); err != nil {
		return admin.User{}, fmt.Errorf("lock activation state: %w", err)
	}
	authz, err := s.repo.FindAuthorizationForUpdate(ctx, tx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return admin.User{}, ErrInvalidToken
		}
		return admin.User{}, fmt.Errorf("find activation authorization: %w", err)
	}
	if authz.Status != "pending" {
		return admin.User{}, ErrAlreadyActivated
	}
	if authz.ActivationExpiresAt == nil || !s.now().Before(*authz.ActivationExpiresAt) ||
		len(authz.ActivationTokenHash) == 0 ||
		subtle.ConstantTimeCompare(authz.ActivationTokenHash, tokenHash) != 1 {
		return admin.User{}, ErrInvalidToken
	}

	if err := s.repo.LockAdminUsersTx(ctx, tx); err != nil {
		return admin.User{}, fmt.Errorf("lock administrator table: %w", err)
	}
	count, err := s.repo.CountAdminsTx(ctx, tx)
	if err != nil {
		return admin.User{}, fmt.Errorf("inspect administrators: %w", err)
	}
	if count > 0 {
		return admin.User{}, ErrAdminExists
	}

	user, err = s.adminRepo.CreateUserTx(ctx, tx, authz.Email, hash, authz.DisplayName, admin.Role(authz.Role))
	if err != nil {
		return admin.User{}, fmt.Errorf("create administrator: %w", err)
	}
	now := s.now()
	if err := s.repo.InvalidateCodesTx(ctx, tx, authz.ID, now); err != nil {
		return admin.User{}, fmt.Errorf("invalidate activation codes: %w", err)
	}
	if err := s.repo.MarkActivatedTx(ctx, tx, authz.ID, user.ID, now); err != nil {
		return admin.User{}, fmt.Errorf("mark authorization activated: %w", err)
	}
	if err := s.adminRepo.InsertAuditEventTx(ctx, tx, &user.ID, "admin.activation.completed",
		map[string]any{"role": string(user.Role)}); err != nil {
		return admin.User{}, fmt.Errorf("record activation audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return admin.User{}, fmt.Errorf("commit activation: %w", err)
	}
	return user, nil
}

// remainingWait returns how long a window anchored at anchor still has to run.
// A missing anchor falls back to the full window so Retry-After is never zero.
func remainingWait(anchor time.Time, window time.Duration, now time.Time) time.Duration {
	if anchor.IsZero() {
		return window
	}
	return anchor.Add(window).Sub(now)
}

func validEmail(email string) bool {
	if email == "" || len(email) > 320 || strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	at := strings.LastIndex(email, "@")
	return at > 0 && at < len(email)-1 && !strings.Contains(email[at+1:], "@")
}

func truncateMeta(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 512 {
		return v[:512]
	}
	return v
}
