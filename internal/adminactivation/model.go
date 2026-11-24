// Package adminactivation implements the one-time, email-verified activation
// of the initial AutivaShop super administrator. It deliberately has no public
// registration surface and no way to change the authorized email address.
package adminactivation

import (
	"context"
	"errors"
	"time"
)

const (
	// ActivationCodeTTL is the fixed lifetime of a six-digit email code.
	ActivationCodeTTL = 5 * time.Minute
	// InitialRole is the only role an activation can ever create.
	InitialRole = "super_admin"
)

var (
	ErrUnconfigured        = errors.New("admin activation is not configured")
	ErrNotAuthorized       = errors.New("email is not authorized for activation")
	ErrNoPendingActivation = errors.New("no pending administrator activation")
	ErrAlreadyActivated    = errors.New("administrator activation is already complete")
	ErrAdminExists         = errors.New("administrator accounts already exist")
	ErrRateLimited         = errors.New("activation rate limit reached")
	ErrEmailDelivery       = errors.New("activation email delivery failed")
	ErrInvalidCode         = errors.New("invalid or expired activation code")
	ErrInvalidToken        = errors.New("invalid activation authorization")
	ErrPasswordWeak        = errors.New("password does not meet requirements")
	ErrInternal            = errors.New("activation could not be completed")
)

// RateLimitError carries the actual remaining lockout so the HTTP layer can
// emit an accurate Retry-After instead of a fixed guess. The message and error
// identity deliberately match ErrRateLimited: no throttle reason is exposed.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// RetryAfterSeconds rounds the remaining wait up to whole seconds. Callers
// treat a non-positive result as "no header available".
func (e *RateLimitError) RetryAfterSeconds() int64 {
	if e == nil || e.RetryAfter <= 0 {
		return 0
	}
	seconds := int64((e.RetryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func newRateLimitError(remaining time.Duration) error {
	if remaining < time.Second {
		remaining = time.Second
	}
	return &RateLimitError{RetryAfter: remaining}
}

// RetryAfterSeconds extracts a safe Retry-After value from a rate-limit error.
// It returns 0 when the error carries no trustworthy remaining duration.
func RetryAfterSeconds(err error) int64 {
	var limited *RateLimitError
	if errors.As(err, &limited) {
		return limited.RetryAfterSeconds()
	}
	return 0
}

// Authorization is a provisioned, one-time activation record. It is not an
// administrator account and cannot be used to sign in.
type Authorization struct {
	ID                  string
	Email               string
	DisplayName         string
	Role                string
	Status              string
	ActivationTokenHash []byte
	ActivationExpiresAt *time.Time
	ActivatedUserID     *string
	CreatedAt           time.Time
}

// ActivationCode is a consumed-or-live email verification attempt. Only a
// keyed hash of the code is ever persisted.
type ActivationCode struct {
	ID              string
	AuthorizationID string
	CodeHash        []byte
	ExpiresAt       time.Time
	FailedAttempts  int
	CreatedAt       time.Time
}

// EmailSender is the narrow delivery boundary. Implementations must return an
// error whenever delivery is not confirmed; they must never fabricate success.
// The code is passed only to the implementation and must never be logged.
type EmailSender interface {
	SendActivationCode(ctx context.Context, to, code string) error
}
