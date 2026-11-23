// Package customer implements customer accounts and private customer sessions.
// It is intentionally independent of administrator identity and privileges.
package customer

import "time"

// AccountStatus is the only account state implemented in Phase 06.
const StatusActive = "active"

// Customer is a persisted account. PasswordHash and raw session tokens are
// never serialized by the API.
type Customer struct {
	ID           string
	Name         string
	Email        string
	PhoneE164    string
	PhoneDisplay string
	PasswordHash string
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

// Session is a persisted customer session. Only token hashes are stored.
type Session struct {
	ID         string
	CustomerID string
	TokenHash  []byte
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastSeenAt *time.Time
	CreatedIP  *string
	UserAgent  *string
}

// Profile is the safe account projection returned by authentication endpoints.
// fullName matches the frontend's transport-neutral account contract.
type Profile struct {
	ID          string     `json:"id"`
	FullName    string     `json:"fullName"`
	Email       string     `json:"email"`
	Phone       string     `json:"phone"`
	PhoneE164   string     `json:"phoneE164"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}

func (c Customer) Public() Profile {
	return Profile{
		ID:          c.ID,
		FullName:    c.Name,
		Email:       c.Email,
		Phone:       c.PhoneDisplay,
		PhoneE164:   c.PhoneE164,
		Status:      c.Status,
		CreatedAt:   c.CreatedAt,
		LastLoginAt: c.LastLoginAt,
	}
}

// RegistrationInput mirrors the storefront account fields plus the Rwanda
// phone number needed for delivery contact snapshots.
type RegistrationInput struct {
	FullName        string `json:"fullName"`
	Email           string `json:"email"`
	Phone           string `json:"phone"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirmPassword,omitempty"`
}

// LoginMeta contains truncated, non-sensitive request metadata for audit rows.
type LoginMeta struct {
	IP        string
	UserAgent string
}
