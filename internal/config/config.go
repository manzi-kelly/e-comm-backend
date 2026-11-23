// Package config loads and validates backend configuration from environment variables.
// It never logs or embeds connection strings, passwords, or credentials.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvProduction  = "production"

	maxRequestBodyBytes = int64(64 << 20) // 64 MiB hard ceiling
)

// Config is the complete, validated application configuration.
type Config struct {
	Environment         string
	Host                string
	Port                int
	Database            DatabaseConfig
	CORS                CORSConfig
	Auth                AuthConfig
	CustomerAuth        CustomerAuthConfig
	Orders              OrdersConfig
	Payment             PaymentConfig
	Timeouts            Timeouts
	MaxRequestBodyBytes int64
	LogLevel            string
	MigrationsDir       string
	ProductImageDir     string
}

// AuthConfig controls admin session cookies, CSRF, login throttling, and bootstrap.
type AuthConfig struct {
	SessionTTL           time.Duration
	SessionCookieName    string
	CSRFCookieName       string
	CookieDomain         string
	CookieSecure         bool
	CookieSameSite       string
	CSRFHeaderName       string
	LoginRateLimitPerMin int
	LoginRateLimitBurst  int
	LoginLockoutWindow   time.Duration
	LoginMaxFailures     int
	BootstrapAllowed     bool
}

// CustomerAuthConfig is deliberately separate from AuthConfig. Customer
// sessions use different cookie names, a different database table and a
// different request principal; they can never satisfy admin authorization.
type CustomerAuthConfig struct {
	SessionTTL        time.Duration
	SessionCookieName string
	CSRFCookieName    string
}

// OrdersConfig controls the finite lifetime of unpaid-order stock reservations.
type OrdersConfig struct {
	ReservationTTL time.Duration
}

// PaymentProviderMock and PaymentProviderXendlyPay are the only recognized
// provider names. The live provider is a future integration boundary only.
const (
	PaymentProviderMock      = "mock"
	PaymentProviderXendlyPay = "xendlypay"
)

// PaymentConfig selects the payment provider. Mock payments are restricted to
// explicitly approved development/test environments; no production fallback
// or live credential configuration exists in this phase.
type PaymentConfig struct {
	Provider    string
	Environment string
	IsMock      bool
}

// DatabaseConfig holds non-secret connection settings. The DSN is kept out of
// this struct's logged representation by never rendering Config with %v.
type DatabaseConfig struct {
	URL               string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
	QueryTimeout      time.Duration
}

// CORSConfig holds the exact origin allowlist.
type CORSConfig struct {
	AllowedOrigins   []string
	AllowCredentials bool
}

// Timeouts groups all server-side durations.
type Timeouts struct {
	Request    time.Duration
	DBQuery    time.Duration
	Shutdown   time.Duration
	ReadHeader time.Duration
	Write      time.Duration
}

// Addr returns the listen address for net/http.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// IsProduction reports whether production-only validation is active.
func (c Config) IsProduction() bool {
	return c.Environment == EnvProduction
}

// Load reads and validates configuration. A getter is injectable for tests.
// Validation errors never include secret values.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	var cfg Config
	var err error

	cfg.Environment = strings.ToLower(strings.TrimSpace(getenv("APP_ENV")))
	if cfg.Environment == "" {
		cfg.Environment = EnvDevelopment
	}
	switch cfg.Environment {
	case EnvDevelopment, EnvTest, EnvProduction:
	default:
		return Config{}, fmt.Errorf("APP_ENV must be one of development, test, or production")
	}

	cfg.Host = strings.TrimSpace(getenv("API_HOST"))
	if cfg.Host == "" {
		if cfg.IsProduction() {
			return Config{}, fmt.Errorf("API_HOST is required in production")
		}
		cfg.Host = "127.0.0.1"
	}

	cfg.Port, err = parsePort(getenv("API_PORT"), 8080, "API_PORT")
	if err != nil {
		return Config{}, err
	}

	cfg.Database, err = loadDatabase(getenv, cfg.Environment)
	if err != nil {
		return Config{}, err
	}

	originsRaw := strings.TrimSpace(getenv("CORS_ORIGINS"))
	if originsRaw == "" {
		if cfg.IsProduction() {
			return Config{}, fmt.Errorf("CORS_ORIGINS is required in production")
		}
		originsRaw = "http://localhost:3000,http://127.0.0.1:3000"
	}
	cfg.CORS.AllowedOrigins, err = parseOrigins(originsRaw)
	if err != nil {
		return Config{}, err
	}
	cfg.CORS.AllowCredentials, err = parseBool(getenv("CORS_ALLOW_CREDENTIALS"), false, "CORS_ALLOW_CREDENTIALS")
	if err != nil {
		return Config{}, err
	}
	if cfg.CORS.AllowCredentials && cfg.IsProduction() && len(cfg.CORS.AllowedOrigins) == 0 {
		return Config{}, fmt.Errorf("CORS_ALLOW_CREDENTIALS requires an explicit CORS origin allowlist")
	}

	cfg.Timeouts.Request, err = parseDuration(getenv("REQUEST_TIMEOUT"), 10*time.Second, "REQUEST_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	cfg.Database.QueryTimeout, err = parseDuration(getenv("DB_QUERY_TIMEOUT"), 5*time.Second, "DB_QUERY_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	cfg.Timeouts.DBQuery = cfg.Database.QueryTimeout
	cfg.Timeouts.Shutdown, err = parseDuration(getenv("SHUTDOWN_TIMEOUT"), 15*time.Second, "SHUTDOWN_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	cfg.Timeouts.ReadHeader, err = parseDuration(getenv("READ_HEADER_TIMEOUT"), 5*time.Second, "READ_HEADER_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	cfg.Timeouts.Write, err = parseDuration(getenv("WRITE_TIMEOUT"), 10*time.Second, "WRITE_TIMEOUT")
	if err != nil {
		return Config{}, err
	}

	cfg.MaxRequestBodyBytes, err = parseBodyLimit(getenv("MAX_REQUEST_BODY_BYTES"), 1<<20)
	if err != nil {
		return Config{}, err
	}

	cfg.Auth, err = loadAuth(getenv, cfg.Environment)
	if err != nil {
		return Config{}, err
	}
	cfg.CustomerAuth, err = loadCustomerAuth(getenv, cfg.Auth)
	if err != nil {
		return Config{}, err
	}
	cfg.Orders, err = loadOrders(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg.Payment, err = loadPayment(getenv, cfg.Environment)
	if err != nil {
		return Config{}, err
	}

	cfg.LogLevel = strings.ToLower(strings.TrimSpace(getenv("LOG_LEVEL")))
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, or error")
	}

	cfg.MigrationsDir = strings.TrimSpace(getenv("MIGRATIONS_DIR"))
	if cfg.MigrationsDir == "" {
		cfg.MigrationsDir = "migrations"
	}

	cfg.ProductImageDir = strings.TrimSpace(getenv("PRODUCT_IMAGE_DIR"))
	if cfg.ProductImageDir == "" {
		cfg.ProductImageDir = "var/product-images"
	}
	return cfg, nil
}

func loadDatabase(getenv func(string) string, environment string) (DatabaseConfig, error) {
	var cfg DatabaseConfig
	var err error

	rawURL := strings.TrimSpace(getenv("DATABASE_URL"))
	explicitURL := rawURL != ""
	if !explicitURL {
		if environment == EnvProduction {
			return DatabaseConfig{}, fmt.Errorf("DATABASE_URL is required in production")
		}
		host := valueOr(getenv("DB_HOST"), "127.0.0.1")
		port, portErr := parsePort(getenv("DB_PORT"), 5432, "DB_PORT")
		if portErr != nil {
			return DatabaseConfig{}, portErr
		}
		user := valueOr(getenv("DB_USER"), "postgres")
		name := valueOr(getenv("DB_NAME"), "autivashop_dev")
		sslmode := valueOr(getenv("DB_SSLMODE"), "disable")
		u := &url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(user, getenv("DB_PASSWORD")),
			Host:   net.JoinHostPort(host, strconv.Itoa(port)),
			Path:   "/" + name,
		}
		q := u.Query()
		q.Set("sslmode", sslmode)
		u.RawQuery = q.Encode()
		rawURL = u.String()
	}

	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil {
		return DatabaseConfig{}, fmt.Errorf("DATABASE_URL must be a valid PostgreSQL URL")
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return DatabaseConfig{}, fmt.Errorf("DATABASE_URL must use the postgres or postgresql scheme")
	}
	if parsed.Host == "" || strings.TrimPrefix(parsed.Path, "/") == "" {
		return DatabaseConfig{}, fmt.Errorf("DATABASE_URL must include a host and database name")
	}

	sslmode := parsed.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "prefer"
	}
	if environment == EnvProduction && (sslmode == "disable" || sslmode == "allow" || sslmode == "prefer") {
		return DatabaseConfig{}, fmt.Errorf("DATABASE_URL must use a verifying TLS mode in production (require, verify-ca, or verify-full)")
	}
	if environment == EnvProduction && !explicitURL {
		return DatabaseConfig{}, fmt.Errorf("DATABASE_URL is required in production")
	}

	cfg.URL = rawURL

	cfg.MaxConns, err = parsePoolInt32(getenv("DB_MAX_CONNS"), 10, "DB_MAX_CONNS", 1, 1000)
	if err != nil {
		return DatabaseConfig{}, err
	}
	cfg.MinConns, err = parsePoolInt32(getenv("DB_MIN_CONNS"), 0, "DB_MIN_CONNS", 0, 1000)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if cfg.MinConns > cfg.MaxConns {
		return DatabaseConfig{}, fmt.Errorf("DB_MIN_CONNS must not exceed DB_MAX_CONNS")
	}
	cfg.MaxConnLifetime, err = parseDuration(getenv("DB_MAX_CONN_LIFETIME"), time.Hour, "DB_MAX_CONN_LIFETIME")
	if err != nil {
		return DatabaseConfig{}, err
	}
	cfg.MaxConnIdleTime, err = parseDuration(getenv("DB_MAX_CONN_IDLE_TIME"), 10*time.Minute, "DB_MAX_CONN_IDLE_TIME")
	if err != nil {
		return DatabaseConfig{}, err
	}
	cfg.HealthCheckPeriod, err = parseDuration(getenv("DB_HEALTH_CHECK_PERIOD"), time.Minute, "DB_HEALTH_CHECK_PERIOD")
	if err != nil {
		return DatabaseConfig{}, err
	}
	return cfg, nil
}

func loadAuth(getenv func(string) string, environment string) (AuthConfig, error) {
	var cfg AuthConfig
	var err error

	cfg.SessionTTL, err = parseDuration(getenv("AUTH_SESSION_TTL"), 8*time.Hour, "AUTH_SESSION_TTL")
	if err != nil {
		return AuthConfig{}, err
	}
	if cfg.SessionTTL < 5*time.Minute || cfg.SessionTTL > 24*time.Hour {
		return AuthConfig{}, fmt.Errorf("AUTH_SESSION_TTL must be between 5m and 24h")
	}

	cfg.SessionCookieName = valueOr(getenv("AUTH_SESSION_COOKIE_NAME"), "autiva_admin_session")
	if !validCookieName(cfg.SessionCookieName) {
		return AuthConfig{}, fmt.Errorf("AUTH_SESSION_COOKIE_NAME must be a valid cookie name")
	}
	cfg.CSRFCookieName = valueOr(getenv("AUTH_CSRF_COOKIE_NAME"), "autiva_admin_csrf")
	if !validCookieName(cfg.CSRFCookieName) {
		return AuthConfig{}, fmt.Errorf("AUTH_CSRF_COOKIE_NAME must be a valid cookie name")
	}
	if cfg.SessionCookieName == cfg.CSRFCookieName {
		return AuthConfig{}, fmt.Errorf("AUTH_SESSION_COOKIE_NAME and AUTH_CSRF_COOKIE_NAME must differ")
	}

	cfg.CookieDomain = strings.TrimSpace(getenv("AUTH_COOKIE_DOMAIN"))
	if cfg.CookieDomain != "" && !validCookieDomain(cfg.CookieDomain) {
		return AuthConfig{}, fmt.Errorf("AUTH_COOKIE_DOMAIN must be a valid host name or be empty")
	}

	cfg.CookieSecure, err = parseBool(getenv("AUTH_COOKIE_SECURE"), environment == EnvProduction, "AUTH_COOKIE_SECURE")
	if err != nil {
		return AuthConfig{}, err
	}
	if environment == EnvProduction && !cfg.CookieSecure {
		return AuthConfig{}, fmt.Errorf("AUTH_COOKIE_SECURE must be true in production")
	}

	cfg.CookieSameSite = strings.ToLower(valueOr(getenv("AUTH_COOKIE_SAMESITE"), "lax"))
	switch cfg.CookieSameSite {
	case "lax", "strict", "none":
	default:
		return AuthConfig{}, fmt.Errorf("AUTH_COOKIE_SAMESITE must be lax, strict, or none")
	}
	if cfg.CookieSameSite == "none" && !cfg.CookieSecure {
		return AuthConfig{}, fmt.Errorf("AUTH_COOKIE_SAMESITE=none requires AUTH_COOKIE_SECURE=true")
	}

	cfg.CSRFHeaderName = valueOr(getenv("AUTH_CSRF_HEADER_NAME"), "X-CSRF-Token")
	if strings.TrimSpace(cfg.CSRFHeaderName) == "" {
		return AuthConfig{}, fmt.Errorf("AUTH_CSRF_HEADER_NAME must not be empty")
	}

	cfg.LoginRateLimitPerMin, err = parseIntRange(getenv("AUTH_LOGIN_RATE_LIMIT_PER_MIN"), 10, "AUTH_LOGIN_RATE_LIMIT_PER_MIN", 1, 10000)
	if err != nil {
		return AuthConfig{}, err
	}
	cfg.LoginRateLimitBurst, err = parseIntRange(getenv("AUTH_LOGIN_RATE_LIMIT_BURST"), 5, "AUTH_LOGIN_RATE_LIMIT_BURST", 1, 10000)
	if err != nil {
		return AuthConfig{}, err
	}
	cfg.LoginMaxFailures, err = parseIntRange(getenv("AUTH_LOGIN_MAX_FAILURES"), 10, "AUTH_LOGIN_MAX_FAILURES", 1, 10000)
	if err != nil {
		return AuthConfig{}, err
	}
	cfg.LoginLockoutWindow, err = parseDuration(getenv("AUTH_LOGIN_LOCKOUT_WINDOW"), 15*time.Minute, "AUTH_LOGIN_LOCKOUT_WINDOW")
	if err != nil {
		return AuthConfig{}, err
	}

	cfg.BootstrapAllowed, err = parseBool(getenv("AUTIVASHOP_ALLOW_BOOTSTRAP"), false, "AUTIVASHOP_ALLOW_BOOTSTRAP")
	if err != nil {
		return AuthConfig{}, err
	}

	return cfg, nil
}

func loadCustomerAuth(getenv func(string) string, admin AuthConfig) (CustomerAuthConfig, error) {
	var cfg CustomerAuthConfig
	var err error
	cfg.SessionTTL, err = parseDuration(getenv("CUSTOMER_SESSION_TTL"), 8*time.Hour, "CUSTOMER_SESSION_TTL")
	if err != nil {
		return CustomerAuthConfig{}, err
	}
	if cfg.SessionTTL < 5*time.Minute || cfg.SessionTTL > 24*time.Hour {
		return CustomerAuthConfig{}, fmt.Errorf("CUSTOMER_SESSION_TTL must be between 5m and 24h")
	}
	cfg.SessionCookieName = valueOr(getenv("CUSTOMER_SESSION_COOKIE_NAME"), "autiva_customer_session")
	if !validCookieName(cfg.SessionCookieName) {
		return CustomerAuthConfig{}, fmt.Errorf("CUSTOMER_SESSION_COOKIE_NAME must be a valid cookie name")
	}
	cfg.CSRFCookieName = valueOr(getenv("CUSTOMER_CSRF_COOKIE_NAME"), "autiva_customer_csrf")
	if !validCookieName(cfg.CSRFCookieName) {
		return CustomerAuthConfig{}, fmt.Errorf("CUSTOMER_CSRF_COOKIE_NAME must be a valid cookie name")
	}
	if cfg.SessionCookieName == cfg.CSRFCookieName || cfg.SessionCookieName == admin.SessionCookieName ||
		cfg.SessionCookieName == admin.CSRFCookieName || cfg.CSRFCookieName == admin.SessionCookieName ||
		cfg.CSRFCookieName == admin.CSRFCookieName {
		return CustomerAuthConfig{}, fmt.Errorf("customer and administrator session/CSRF cookie names must all differ")
	}
	return cfg, nil
}

func loadOrders(getenv func(string) string) (OrdersConfig, error) {
	ttl, err := parseDuration(getenv("ORDER_RESERVATION_TTL"), 30*time.Minute, "ORDER_RESERVATION_TTL")
	if err != nil {
		return OrdersConfig{}, err
	}
	if ttl < 5*time.Minute || ttl > 4*time.Hour {
		return OrdersConfig{}, fmt.Errorf("ORDER_RESERVATION_TTL must be between 5m and 4h")
	}
	return OrdersConfig{ReservationTTL: ttl}, nil
}

// loadPayment is fail-closed. Mock is never available in production, and the
// future live provider is a typed boundary that cannot initiate payments yet.
func loadPayment(getenv func(string) string, environment string) (PaymentConfig, error) {
	provider := strings.ToLower(strings.TrimSpace(getenv("PAYMENT_PROVIDER")))
	if provider == "" {
		if environment == EnvProduction {
			return PaymentConfig{}, fmt.Errorf("PAYMENT_PROVIDER is required in production; mock payments are never available in production")
		}
		provider = PaymentProviderMock
	}
	switch provider {
	case PaymentProviderMock:
		if environment == EnvProduction {
			return PaymentConfig{}, fmt.Errorf("PAYMENT_PROVIDER=mock is forbidden in production")
		}
		return PaymentConfig{Provider: provider, Environment: environment, IsMock: true}, nil
	case PaymentProviderXendlyPay:
		if environment == EnvProduction {
			return PaymentConfig{}, fmt.Errorf("PAYMENT_PROVIDER=xendlypay is a future boundary and is not implemented; production payments fail closed")
		}
		return PaymentConfig{Provider: provider, Environment: "test", IsMock: false}, nil
	default:
		return PaymentConfig{}, fmt.Errorf("PAYMENT_PROVIDER must be mock or xendlypay")
	}
}

func parseIntRange(raw string, fallback int, name string, min, max int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, min, max)
	}
	return value, nil
}

func validCookieName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func validCookieDomain(domain string) bool {
	if len(domain) > 255 || strings.ContainsAny(domain, " /\\,:;") {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	// Allow localhost and normal hostnames; IPs are unusual for cookies but accept literal hosts.
	for _, c := range domain {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// RedactDSN removes passwords from a URL-style PostgreSQL DSN for safe display.
func RedactDSN(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "[redacted-dsn]"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}
	q := u.Query()
	if q.Has("password") {
		q.Set("password", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func valueOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func parsePort(raw string, fallback int, name string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be an integer between 1 and 65535", name)
	}
	return port, nil
}

func parsePoolInt32(raw string, fallback int32, name string, min, max int32) (int32, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil || int32(value) < min || int32(value) > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, min, max)
	}
	return int32(value), nil
}

func parseDuration(raw string, fallback time.Duration, name string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration (for example 10s)", name)
	}
	return value, nil
}

func parseBool(raw string, fallback bool, name string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

func parseBodyLimit(raw string, fallback int64) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 || value > maxRequestBodyBytes {
		return 0, fmt.Errorf("MAX_REQUEST_BODY_BYTES must be between 1 and %d", maxRequestBodyBytes)
	}
	return value, nil
}

func parseOrigins(raw string) ([]string, error) {
	seen := make(map[string]struct{})
	origins := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(item)
		if origin == "" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u == nil {
			return nil, fmt.Errorf("CORS_ORIGINS contains an invalid origin")
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" ||
			strings.Contains(u.Host, "*") {
			return nil, fmt.Errorf("CORS_ORIGINS entries must be exact http://host[:port] or https://host[:port] values")
		}
		if _, exists := seen[origin]; !exists {
			seen[origin] = struct{}{}
			origins = append(origins, origin)
		}
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("CORS_ORIGINS must contain at least one origin")
	}
	return origins, nil
}
