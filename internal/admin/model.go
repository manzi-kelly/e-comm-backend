package admin

import "time"

// Role identifies an administrator privilege level.
type Role string

const (
	RoleSuperAdmin Role = "super_admin"
	RoleAdmin      Role = "admin"
)

func (r Role) Valid() bool {
	return r == RoleSuperAdmin || r == RoleAdmin
}

// User is an administrator account record. PasswordHash and other secrets must never be serialized to API responses.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	DisplayName  string
	Role         Role
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

// Session is a persisted administrator session.
type Session struct {
	ID          string
	AdminUserID string
	TokenHash   []byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
	LastSeenAt  *time.Time
	CreatedIP   *string
	UserAgent   *string
}

// AuthenticatedUser is the safe profile exposed by authentication endpoints.
type AuthenticatedUser struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Role        Role       `json:"role"`
	IsActive    bool       `json:"isActive"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}

func (u User) Public() AuthenticatedUser {
	return AuthenticatedUser{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		IsActive:    u.IsActive,
		LastLoginAt: u.LastLoginAt,
	}
}

// AuditEvent is a security-relevant event record.
type AuditEvent struct {
	ID          string
	AdminUserID *string
	Action      string
	OccurredAt  time.Time
	Metadata    map[string]any
}
