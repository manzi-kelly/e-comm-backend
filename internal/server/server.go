// Package server wires HTTP routing, middleware, lifecycle and the two
// infrastructure endpoints. It contains no business routes in Phase 01.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/customer"
	"autivashop/backend/internal/middleware"
	"autivashop/backend/internal/orders"
	"autivashop/backend/internal/payment"
	"autivashop/backend/internal/product"
)

// Pinger is the narrow database capability needed by the readiness endpoint.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server owns the HTTP server and its configuration.
type Server struct {
	cfg             config.Config
	db              Pinger
	adminService    *admin.Service
	productService  *product.Service
	customerService *customer.Service
	orderService    *orders.Service
	paymentService  *payment.Service
	logger          *slog.Logger
	httpServer      *http.Server
	startedAt       time.Time
}

// New builds a fully configured server, including its middleware chain.
func New(cfg config.Config, db Pinger, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:       cfg,
		db:        db,
		logger:    logger,
		startedAt: time.Now().UTC(),
	}
	s.httpServer = &http.Server{
		Addr:              cfg.Addr(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader,
		WriteTimeout:      cfg.Timeouts.Write,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return s
}

// SetAdminService enables administrator authentication routes and middleware.
func (s *Server) SetAdminService(service *admin.Service) {
	s.adminService = service
	if s.httpServer != nil {
		s.httpServer.Handler = s.Handler()
	}
}

func (s *Server) SetProductService(service *product.Service) {
	s.productService = service
	s.httpServer.Handler = s.Handler()
}

// SetCustomerService enables private customer authentication routes. Its
// cookies, principal type and session table are independent of admin auth.
func (s *Server) SetCustomerService(service *customer.Service) {
	s.customerService = service
	s.httpServer.Handler = s.Handler()
}

// SetOrderService enables owner-scoped order routes. Orders remain unavailable
// unless a customer service is also registered.
func (s *Server) SetOrderService(service *orders.Service) {
	s.orderService = service
	s.httpServer.Handler = s.Handler()
}

// SetPaymentService enables the local payment surface. Customer routes also
// require a customer service; admin routes also require an admin service.
func (s *Server) SetPaymentService(service *payment.Service) {
	s.paymentService = service
	s.httpServer.Handler = s.Handler()
}

// Handler returns the composed HTTP handler. Middleware order is:
// request ID → panic recovery → logging → headers → CORS → body limit →
// request timeout → router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("/api/v1/health", middleware.MethodNotAllowed)
	mux.HandleFunc("GET /api/v1/ready", s.handleReady)
	mux.HandleFunc("/api/v1/ready", middleware.MethodNotAllowed)

	if s.adminService != nil {
		cookieCfg := auth.NewCookieConfig(s.cfg.Auth)
		ah := &adminHandlers{
			svc:     s.adminService,
			cfg:     s.cfg,
			cookies: cookieCfg,
			logger:  s.logger,
		}
		mux.HandleFunc("POST /api/v1/admin/auth/login", ah.login)
		mux.HandleFunc("/api/v1/admin/auth/login", middleware.MethodNotAllowed)
		mux.HandleFunc("POST /api/v1/admin/auth/logout", ah.requireAuth(ah.logout))
		mux.HandleFunc("/api/v1/admin/auth/logout", middleware.MethodNotAllowed)
		mux.HandleFunc("GET /api/v1/admin/auth/me", ah.me)
		mux.HandleFunc("/api/v1/admin/auth/me", middleware.MethodNotAllowed)
		mux.HandleFunc("POST /api/v1/admin/auth/logout-all", ah.requireAuth(ah.logoutAll))
		mux.HandleFunc("/api/v1/admin/auth/logout-all", middleware.MethodNotAllowed)

		// Protected diagnostic endpoint for authorization/session verification only.
		mux.HandleFunc("GET /api/v1/admin/auth/session-check", ah.requireAuth(ah.sessionCheck))
		mux.HandleFunc("/api/v1/admin/auth/session-check", middleware.MethodNotAllowed)
	}

	if s.customerService != nil {
		s.registerCustomerAuth(mux)
	}
	if s.customerService != nil && s.orderService != nil {
		s.registerOrders(mux)
	}
	if s.paymentService != nil {
		s.registerPayments(mux)
	}

	s.registerProducts(mux)
	notFound := func(w http.ResponseWriter, r *http.Request) {
		middleware.WriteError(w, r, http.StatusNotFound, middleware.CodeNotFound,
			"Endpoint not found.", nil)
	}
	mux.HandleFunc("/api/v1/", notFound)
	mux.HandleFunc("/", notFound)

	return middleware.Chain(mux,
		middleware.RequestID,
		middleware.Recover(s.logger),
		middleware.Logging(s.logger),
		middleware.SecurityHeaders,
		middleware.CORS(s.cfg.CORS.AllowedOrigins, s.cfg.CORS.AllowCredentials),
		s.productBodyLimit,
		middleware.RequestTimeout(s.cfg.Timeouts.Request),
	)
}

// Run opens the configured TCP listener and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr())
	if err != nil {
		return err
	}
	return s.Serve(ctx, listener)
}

// Serve serves an existing listener and performs graceful shutdown on ctx
// cancellation. It is separated from Run so shutdown can be tested without a
// real network port.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.logger.Info("shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.Shutdown)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("graceful shutdown failed")
			_ = s.httpServer.Close()
			return err
		}
		err := <-serveErr
		if errors.Is(err, http.ErrServerClosed) {
			s.logger.Info("shutdown complete")
			return nil
		}
		return err
	}
}
