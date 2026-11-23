package auth

import "context"

type contextKey string

const (
	adminUserKey       contextKey = "admin_user"
	adminSessionKey    contextKey = "admin_session"
	customerUserKey    contextKey = "customer_user"
	customerSessionKey contextKey = "customer_session"
)

// AdminPrincipal is the minimal authenticated identity attached to a request context.
type AdminPrincipal struct {
	ID          string
	Email       string
	DisplayName string
	Role        string
	IsActive    bool
	SessionID   string
}

func WithPrincipal(ctx context.Context, p AdminPrincipal) context.Context {
	return context.WithValue(ctx, adminUserKey, p)
}

func PrincipalFromContext(ctx context.Context) (AdminPrincipal, bool) {
	p, ok := ctx.Value(adminUserKey).(AdminPrincipal)
	return p, ok
}

// CustomerPrincipal is a separate identity type. It never carries an admin
// role and is never written to the admin principal context key.
type CustomerPrincipal struct {
	ID        string
	Name      string
	Email     string
	PhoneE164 string
	SessionID string
}

func WithCustomerPrincipal(ctx context.Context, p CustomerPrincipal) context.Context {
	return context.WithValue(ctx, customerUserKey, p)
}

func CustomerPrincipalFromContext(ctx context.Context) (CustomerPrincipal, bool) {
	p, ok := ctx.Value(customerUserKey).(CustomerPrincipal)
	return p, ok
}

func WithCustomerSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, customerSessionKey, id)
}

func CustomerSessionFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(customerSessionKey).(string)
	return id, ok
}
