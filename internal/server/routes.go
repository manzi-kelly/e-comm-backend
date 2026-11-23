package server

import (
	"net/http"

	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/middleware"
)

// methodFallback gives known paths a consistent 405 response without changing
// existing endpoint behavior.
func methodFallback(mux *http.ServeMux, pattern, allow string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		middleware.WriteError(w, r, http.StatusMethodNotAllowed, middleware.CodeMethodNotAllowed,
			"Method not allowed for this endpoint.", nil)
	})
}

func (s *Server) registerCustomerAuth(mux *http.ServeMux) {
	h := &customerHandlers{
		svc:     s.customerService,
		cfg:     s.cfg,
		cookies: auth.NewCustomerCookieConfig(s.cfg),
		logger:  s.logger,
	}
	mux.HandleFunc("POST /api/v1/customer/auth/register", h.register)
	methodFallback(mux, "/api/v1/customer/auth/register", "POST")

	mux.HandleFunc("POST /api/v1/customer/auth/login", h.login)
	methodFallback(mux, "/api/v1/customer/auth/login", "POST")

	mux.HandleFunc("GET /api/v1/customer/auth/me", h.me)
	methodFallback(mux, "/api/v1/customer/auth/me", "GET, HEAD")

	mux.HandleFunc("POST /api/v1/customer/auth/logout", h.requireAuth(h.logout))
	methodFallback(mux, "/api/v1/customer/auth/logout", "POST")
}

func (s *Server) registerOrders(mux *http.ServeMux) {
	customerHandlers := &customerHandlers{
		svc:     s.customerService,
		cfg:     s.cfg,
		cookies: auth.NewCustomerCookieConfig(s.cfg),
		logger:  s.logger,
	}
	h := &orderHandlers{svc: s.orderService}
	protect := func(f http.HandlerFunc) http.HandlerFunc {
		return customerHandlers.requireAuth(
			middleware.RequireCustomer(f).ServeHTTP,
		)
	}
	mux.HandleFunc("POST /api/v1/orders", protect(h.create))
	methodFallback(mux, "/api/v1/orders", "GET, HEAD, POST")

	mux.HandleFunc("GET /api/v1/orders", protect(h.list))
	mux.HandleFunc("GET /api/v1/orders/{id}", protect(h.get))
	methodFallback(mux, "/api/v1/orders/{id}", "GET, HEAD")
}

// registerPayments keeps mock payment controls out of production and keeps the
// customer, developer-simulation and admin surfaces behind separate principals.
func (s *Server) registerPayments(mux *http.ServeMux) {
	h := &paymentHandlers{svc: s.paymentService, logger: s.logger}

	if s.customerService != nil {
		ch := &customerHandlers{
			svc:     s.customerService,
			cfg:     s.cfg,
			cookies: auth.NewCustomerCookieConfig(s.cfg),
			logger:  s.logger,
		}
		protect := func(f http.HandlerFunc) http.HandlerFunc {
			return ch.requireAuth(middleware.RequireCustomer(f).ServeHTTP)
		}
		mux.HandleFunc("POST /api/v1/orders/{id}/payments", protect(h.initiate))
		mux.HandleFunc("GET /api/v1/orders/{id}/payments", protect(h.listForOrder))
		methodFallback(mux, "/api/v1/orders/{id}/payments", "GET, HEAD, POST")

		mux.HandleFunc("GET /api/v1/orders/{id}/payments/{paymentId}", protect(h.get))
		methodFallback(mux, "/api/v1/orders/{id}/payments/{paymentId}", "GET, HEAD")

		if !s.cfg.IsProduction() && s.cfg.Payment.IsMock {
			mux.HandleFunc("POST /api/v1/dev/payments/{paymentId}/simulate", protect(h.simulate))
			methodFallback(mux, "/api/v1/dev/payments/{paymentId}/simulate", "POST")
		}
	}

	if s.adminService != nil {
		ah := &adminHandlers{
			svc:     s.adminService,
			cfg:     s.cfg,
			cookies: auth.NewCookieConfig(s.cfg.Auth),
			logger:  s.logger,
		}
		protect := func(f http.HandlerFunc) http.HandlerFunc {
			return ah.requireAuth(middleware.RequireAuthenticated(
				middleware.RequireActiveAccount(
					middleware.RequireRoles("admin", "super_admin")(f),
				),
			).ServeHTTP)
		}
		mux.HandleFunc("GET /api/v1/admin/payments", protect(h.adminList))
		mux.HandleFunc("GET /api/v1/admin/payments/summary", protect(h.adminSummary))
		mux.HandleFunc("GET /api/v1/admin/payments/{attemptId}", protect(h.adminGet))
		methodFallback(mux, "/api/v1/admin/payments", "GET, HEAD")
		// The literal summary path is more specific than the attempt wildcard.
		// A non-method fallback would conflict with "GET .../{attemptId}".
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			methodFallback(mux, method+" /api/v1/admin/payments/summary", "GET, HEAD")
		}
		methodFallback(mux, "/api/v1/admin/payments/{attemptId}", "GET, HEAD")
	}
}
