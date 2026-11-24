// Command api runs the standalone AutivaShop REST API.
// Usage: autivashop-api.exe            starts the HTTP server
//
//	autivashop-api.exe migrate    applies forward-only SQL migrations
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"autivashop/backend/internal/admin"
	"autivashop/backend/internal/adminactivation"
	"autivashop/backend/internal/adminreport"
	"autivashop/backend/internal/auth"
	"autivashop/backend/internal/config"
	"autivashop/backend/internal/customer"
	"autivashop/backend/internal/database"
	"autivashop/backend/internal/fulfillment"
	"autivashop/backend/internal/orders"
	"autivashop/backend/internal/payment"
	"autivashop/backend/internal/product"
	"autivashop/backend/internal/server"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			runMigrate()
		case "bootstrap-admin":
			runBootstrapAdmin()
		case "provision-admin-activation":
			runProvisionAdminActivation()
		case "cleanup-product-images":
			runImageCleanup()
		default:
			fmt.Fprintln(os.Stderr, "unknown command; expected 'migrate', 'bootstrap-admin', 'provision-admin-activation', 'cleanup-product-images', or no argument")
			os.Exit(2)
		}
		return
	}
	runServer()
}

func runServer() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration:", err)
		os.Exit(1)
	}
	logger := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A database outage must not disable the health endpoint: the server starts
	// with a nil readiness dependency, so /api/v1/ready reports 503 until the
	// database is available on a subsequent process start.
	var pinger server.Pinger
	var adminSvc *admin.Service
	var activationSvc *adminactivation.Service
	var adminReportSvc *adminreport.Service
	var productSvc *product.Service
	var customerSvc *customer.Service
	var orderSvc *orders.Service
	var paymentSvc *payment.Service
	var fulfillmentSvc *fulfillment.Service
	if db, err := database.New(ctx, cfg.Database); err != nil {
		logger.Error("database unavailable at startup; readiness will report 503")
	} else {
		defer db.Close()
		pinger = db
		adminSvc = admin.NewService(db.Pool(), cfg, logger)
		activationSvc, err = newActivationService(db.Pool(), cfg, logger)
		if err != nil {
			// Activation endpoints stay registered but fail closed with 503;
			// e-commerce, payments and fulfillment remain unaffected.
			logger.Error("admin activation disabled", "reason", err.Error())
		}
		adminReportSvc = adminreport.NewService(db.Pool())
		customerSvc = customer.NewService(db.Pool(), cfg, logger)
		orderSvc = orders.NewService(db.Pool(), cfg)
		fulfillmentSvc = fulfillment.NewService(db.Pool(), cfg)
		if provider, providerErr := newPaymentProvider(cfg); providerErr != nil {
			logger.Error("payment provider unavailable; payment routes disabled")
		} else if svc, serviceErr := payment.NewService(db.Pool(), provider, cfg); serviceErr != nil {
			logger.Error("payment service unavailable; payment routes disabled")
		} else {
			paymentSvc = svc
			logger.Info("payment provider selected", "provider", provider.Name(), "mock", provider.IsMock())
		}
		var storage product.Storage
		// Local storage is development-only. Production must supply a future
		// durable object adapter; uploads fail closed in the meantime.
		if !cfg.IsProduction() {
			local, storageErr := product.NewLocalStorage(cfg.ProductImageDir)
			if storageErr != nil {
				logger.Error("product image storage unavailable")
			} else {
				defer local.Close()
				storage = local
			}
		}
		productSvc = product.NewService(db.Pool(), storage)
	}

	srv := server.New(cfg, pinger, logger)
	if adminSvc != nil {
		srv.SetAdminService(adminSvc)
	}
	srv.SetAdminActivationService(activationSvc)
	if adminReportSvc != nil {
		srv.SetAdminReportService(adminReportSvc)
	}
	if productSvc != nil {
		srv.SetProductService(productSvc)
	}
	if customerSvc != nil {
		srv.SetCustomerService(customerSvc)
	}
	if orderSvc != nil {
		srv.SetOrderService(orderSvc)
	}
	if paymentSvc != nil {
		srv.SetPaymentService(paymentSvc)
	}
	if fulfillmentSvc != nil {
		srv.SetFulfillmentService(fulfillmentSvc)
	}

	logger.Info("api starting", "address", cfg.Addr(), "environment", cfg.Environment)
	if err := srv.Run(ctx); err != nil {
		logger.Error("api stopped with error")
		os.Exit(1)
	}
	logger.Info("api stopped")
}

func runImageCleanup() {
	cfg, err := config.Load(os.Getenv)
	if err != nil || cfg.IsProduction() {
		fmt.Fprintln(os.Stderr, "local image cleanup requires valid development/test configuration")
		os.Exit(1)
	}
	ctx := context.Background()
	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database unavailable")
		os.Exit(1)
	}
	defer db.Close()
	storage, err := product.NewLocalStorage(cfg.ProductImageDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product storage unavailable")
		os.Exit(1)
	}
	defer storage.Close()
	count, err := product.NewService(db.Pool(), storage).CleanupOrphans(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product image cleanup failed")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "removed %d orphan product image files\n", count)
}

func runMigrate() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration:", err)
		os.Exit(1)
	}
	logger := newLogger(cfg)

	db, err := database.New(context.Background(), cfg.Database)
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	applied, err := db.Migrate(context.Background(), cfg.MigrationsDir)
	if err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
	logger.Info("migrations complete", "applied", applied)
}

// newActivationService requires complete SMTP/HMAC configuration. Missing
// configuration disables activation instead of fabricated delivery success.
func newActivationService(pool *pgxpool.Pool, cfg config.Config, logger *slog.Logger) (*adminactivation.Service, error) {
	ac := cfg.AdminActivation
	if !ac.Enabled {
		issues := strings.Join(ac.SetupIssues, ", ")
		if issues == "" {
			issues = "activation email delivery is not configured"
		}
		return nil, fmt.Errorf("required setup: %s", issues)
	}
	sender, err := adminactivation.NewSMTPSender(ac.SMTPHost, ac.SMTPPort, ac.SMTPUsername,
		ac.SMTPPassword, ac.SMTPFrom, ac.SMTPTLSMode, ac.SMTPTimeout)
	if err != nil {
		return nil, err
	}
	return adminactivation.NewService(pool, ac, sender.WithLogger(logger), logger)
}

// runProvisionAdminActivation is an explicit development/test database setup
// operation. It provisions only the compiled-in authorized email and creates
// no administrator account, password or session.
func runProvisionAdminActivation() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration")
		os.Exit(1)
	}
	if cfg.IsProduction() {
		fmt.Fprintln(os.Stderr, "activation setup is refused in production")
		os.Exit(1)
	}
	if !cfg.AdminActivation.SetupAllowed {
		fmt.Fprintln(os.Stderr, "activation setup is not authorized: set AUTIVASHOP_ALLOW_ACTIVATION_SETUP=true in an explicitly controlled development/test environment")
		os.Exit(1)
	}

	ctx := context.Background()
	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database connection failed")
		os.Exit(1)
	}
	defer db.Close()

	versions, err := db.AppliedVersions(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not inspect applied migrations")
		os.Exit(1)
	}
	hasActivationSchema := false
	for _, version := range versions {
		if version == "0007_admin_activation.sql" {
			hasActivationSchema = true
			break
		}
	}
	if !hasActivationSchema {
		fmt.Fprintln(os.Stderr, "activation schema is missing: run 'migrate' first")
		os.Exit(1)
	}

	displayName := strings.TrimSpace(os.Getenv("ADMIN_ACTIVATION_DISPLAY_NAME"))
	if displayName == "" {
		displayName = "AutivaShop Administrator"
	}
	created, err := adminactivation.NewProvisioner(db.Pool(), newLogger(cfg)).Provision(ctx, displayName)
	if err != nil {
		switch {
		case errors.Is(err, adminactivation.ErrAdminExists):
			fmt.Fprintln(os.Stderr, "activation setup refused: administrator accounts already exist; existing accounts were left untouched")
		case errors.Is(err, adminactivation.ErrAlreadyActivated):
			fmt.Fprintln(os.Stderr, "activation setup refused: initial administrator activation is already complete")
		default:
			fmt.Fprintln(os.Stderr, "activation setup failed")
		}
		os.Exit(1)
	}
	if created {
		fmt.Fprintln(os.Stdout, "authorized administrator email provisioned; no administrator account was created")
		return
	}
	fmt.Fprintln(os.Stdout, "authorized administrator email was already provisioned; no changes made")
}

func runBootstrapAdmin() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration")
		os.Exit(1)
	}
	logger := newLogger(cfg)

	if !cfg.Auth.BootstrapAllowed {
		fmt.Fprintln(os.Stderr, "bootstrap is not authorized: set AUTIVASHOP_ALLOW_BOOTSTRAP=true in an explicitly controlled environment")
		os.Exit(1)
	}
	email := admin.NormalizeEmail(os.Getenv("BOOTSTRAP_ADMIN_EMAIL"))
	displayName := strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_DISPLAY_NAME"))
	password := os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")
	roleRaw := strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_ROLE"))
	if roleRaw == "" {
		roleRaw = string(admin.RoleSuperAdmin)
	}
	role := admin.Role(roleRaw)

	// Allow secure password input from stdin when not supplied via environment.
	if password == "" {
		fmt.Fprint(os.Stderr, "Enter bootstrap administrator password: ")
		reader := bufio.NewReader(os.Stdin)
		line, readErr := reader.ReadString('\n')
		if readErr != nil && len(strings.TrimSpace(line)) == 0 {
			fmt.Fprintln(os.Stderr, "bootstrap password is required")
			os.Exit(1)
		}
		password = strings.TrimRight(line, "\r\n")
	}

	if email == "" || displayName == "" || password == "" || !role.Valid() {
		fmt.Fprintln(os.Stderr, "bootstrap requires BOOTSTRAP_ADMIN_EMAIL, BOOTSTRAP_ADMIN_DISPLAY_NAME, a secure password, and a valid BOOTSTRAP_ADMIN_ROLE")
		os.Exit(1)
	}
	if strings.Contains(email, " ") || !strings.Contains(email, "@") {
		fmt.Fprintln(os.Stderr, "bootstrap email is invalid")
		os.Exit(1)
	}
	if err := auth.ValidatePasswordStrength(password); err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap password does not meet security requirements")
		os.Exit(1)
	}

	ctx := context.Background()
	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database connection failed")
		os.Exit(1)
	}
	defer db.Close()

	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not secure bootstrap password")
		os.Exit(1)
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not start bootstrap transaction")
		os.Exit(1)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	repo := admin.NewRepository(db.Pool())
	count, err := repo.CountUsersTx(ctx, tx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not inspect existing administrators")
		os.Exit(1)
	}
	if count > 0 {
		fmt.Fprintln(os.Stderr, "bootstrap refused: administrator accounts already exist")
		os.Exit(1)
	}
	user, err := repo.CreateUserTx(ctx, tx, email, hash, displayName, role)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			fmt.Fprintln(os.Stderr, "bootstrap refused: administrator account already exists")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "could not create administrator account")
		os.Exit(1)
	}
	uid := user.ID
	if err := repo.InsertAuditEventTx(ctx, tx, &uid, "admin.bootstrap.created", map[string]any{"role": string(user.Role)}); err != nil {
		fmt.Fprintln(os.Stderr, "could not record bootstrap audit event")
		os.Exit(1)
	}
	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "could not complete bootstrap")
		os.Exit(1)
	}

	logger.Info("administrator bootstrap completed", "role", user.Role)
	fmt.Fprintln(os.Stdout, "administrator bootstrap completed")
	// Do not print email, password, or identifiers.
	_ = errors.New // keep errors import used
}

// newPaymentProvider is fail-closed: production has no executable provider in
// Phase 08, and the live XendlyPay boundary can never return simulated success.
func newPaymentProvider(cfg config.Config) (payment.Provider, error) {
	if cfg.IsProduction() {
		return nil, payment.ErrProviderUnavailable
	}
	switch cfg.Payment.Provider {
	case config.PaymentProviderMock:
		return payment.NewMockPaymentProvider(cfg.Payment.Environment), nil
	case config.PaymentProviderXendlyPay:
		return payment.NewXendlyPayProvider(), nil
	default:
		return nil, payment.ErrProviderUnavailable
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
