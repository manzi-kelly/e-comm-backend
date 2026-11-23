package tests

import (
	"strings"
	"testing"

	"autivashop/backend/internal/config"
)

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestDevelopmentDefaults(t *testing.T) {
	cfg, err := config.Load(envFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("Load() with development defaults failed: %v", err)
	}
	if cfg.Environment != config.EnvDevelopment {
		t.Fatalf("environment = %q, want development", cfg.Environment)
	}
	if cfg.Addr() != "127.0.0.1:8080" {
		t.Fatalf("Addr() = %q, want 127.0.0.1:8080", cfg.Addr())
	}
	if cfg.MaxRequestBodyBytes != 1<<20 {
		t.Fatalf("MaxRequestBodyBytes = %d, want 1048576", cfg.MaxRequestBodyBytes)
	}
	if cfg.LogLevel != "info" || cfg.MigrationsDir != "migrations" {
		t.Fatalf("unexpected log level or migrations directory")
	}
	if len(cfg.CORS.AllowedOrigins) != 2 {
		t.Fatalf("default CORS origins = %v, want two loopback origins", cfg.CORS.AllowedOrigins)
	}
	if !strings.Contains(cfg.Database.URL, "autivashop_dev") {
		t.Fatal("development DSN must target the development database name")
	}
}

func TestDiscreteDatabaseSettingsBuildDSN(t *testing.T) {
	cfg, err := config.Load(envFrom(map[string]string{
		"DB_HOST":     "127.0.0.1",
		"DB_PORT":     "5433",
		"DB_USER":     "autivashop_app",
		"DB_PASSWORD": "not-a-real-secret",
		"DB_NAME":     "autivashop_dev",
		"DB_SSLMODE":  "require",
	}))
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if !strings.Contains(cfg.Database.URL, "5433") || !strings.Contains(cfg.Database.URL, "autivashop_dev") {
		t.Fatalf("unexpected DSN: %s", config.RedactDSN(cfg.Database.URL))
	}
}

func TestProductionValidation(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		wantOK bool
	}{
		{
			name: "missing DATABASE_URL",
			env: map[string]string{
				"APP_ENV":      "production",
				"API_HOST":     "0.0.0.0",
				"CORS_ORIGINS": "https://shop.example.com",
			},
		},
		{
			name: "insecure SSL mode",
			env: map[string]string{
				"APP_ENV":      "production",
				"API_HOST":     "0.0.0.0",
				"CORS_ORIGINS": "https://shop.example.com",
				"DATABASE_URL": "postgres://app:SuperSecret123@db.example.com:5432/autivashop?sslmode=disable",
			},
		},
		{
			name: "missing host",
			env: map[string]string{
				"APP_ENV":      "production",
				"CORS_ORIGINS": "https://shop.example.com",
				"DATABASE_URL": "postgres://app:SuperSecret123@db.example.com:5432/autivashop?sslmode=require",
			},
		},
		{
			name: "missing CORS origins",
			env: map[string]string{
				"APP_ENV":      "production",
				"API_HOST":     "0.0.0.0",
				"DATABASE_URL": "postgres://app:SuperSecret123@db.example.com:5432/autivashop?sslmode=require",
			},
		},
		{
			// Phase 08: production fails closed until a live provider is verified.
			name: "production without payment provider",
			env: map[string]string{
				"APP_ENV":      "production",
				"API_HOST":     "0.0.0.0",
				"CORS_ORIGINS": "https://shop.example.com,https://admin.example.com",
				"DATABASE_URL": "postgres://app:SuperSecret123@db.example.com:5432/autivashop?sslmode=verify-full",
			},
		},
		{
			name: "production with mock provider",
			env: map[string]string{
				"APP_ENV":          "production",
				"API_HOST":         "0.0.0.0",
				"CORS_ORIGINS":     "https://shop.example.com",
				"DATABASE_URL":     "postgres://app:SuperSecret123@db.example.com:5432/autivashop?sslmode=verify-full",
				"PAYMENT_PROVIDER": "mock",
			},
		},
		{
			name: "production with unimplemented live provider",
			env: map[string]string{
				"APP_ENV":          "production",
				"API_HOST":         "0.0.0.0",
				"CORS_ORIGINS":     "https://shop.example.com",
				"DATABASE_URL":     "postgres://app:SuperSecret123@db.example.com:5432/autivashop?sslmode=verify-full",
				"PAYMENT_PROVIDER": "xendlypay",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(envFrom(tc.env))
			if tc.wantOK && err != nil {
				t.Fatalf("Load() failed: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("Load() unexpectedly accepted invalid production configuration")
			}
			if err != nil && strings.Contains(err.Error(), "SuperSecret123") {
				t.Fatal("validation error leaked a credential")
			}
		})
	}
}

func TestInvalidConfigurationValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		key  string
	}{
		{"bad environment", map[string]string{"APP_ENV": "staging"}, "APP_ENV"},
		{"bad port", map[string]string{"API_PORT": "70000"}, "API_PORT"},
		{"zero port", map[string]string{"API_PORT": "0"}, "API_PORT"},
		{"wildcard cors", map[string]string{"CORS_ORIGINS": "*"}, "CORS_ORIGINS"},
		{"cors with path", map[string]string{"CORS_ORIGINS": "https://example.com/admin"}, "CORS_ORIGINS"},
		{"bad log level", map[string]string{"LOG_LEVEL": "trace"}, "LOG_LEVEL"},
		{"bad duration", map[string]string{"REQUEST_TIMEOUT": "soon"}, "REQUEST_TIMEOUT"},
		{"zero duration", map[string]string{"DB_QUERY_TIMEOUT": "0s"}, "DB_QUERY_TIMEOUT"},
		{"body too large", map[string]string{"MAX_REQUEST_BODY_BYTES": "999999999"}, "MAX_REQUEST_BODY_BYTES"},
		{"pool inverted", map[string]string{"DB_MIN_CONNS": "20", "DB_MAX_CONNS": "10"}, "DB_MIN_CONNS"},
		{"bad ssl", map[string]string{"DATABASE_URL": "postgres://app:pw@localhost:5432/db?sslmode=disable", "APP_ENV": "production", "API_HOST": "0.0.0.0", "CORS_ORIGINS": "https://example.com"}, "DATABASE_URL"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(envFrom(tc.env))
			if err == nil {
				t.Fatalf("Load() accepted invalid %s", tc.key)
			}
			if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "SuperSecret") {
				t.Fatal("validation error leaked a credential")
			}
		})
	}
}

func TestRedactDSN(t *testing.T) {
	raw := "postgres://app:SuperSecret123@db.example.com:5432/autivashop?sslmode=require&password=another-secret"
	redacted := config.RedactDSN(raw)
	if strings.Contains(redacted, "SuperSecret123") || strings.Contains(redacted, "another-secret") {
		t.Fatalf("RedactDSN leaked a secret: %s", redacted)
	}
	if !strings.Contains(redacted, "REDACTED") {
		t.Fatalf("RedactDSN did not mask the password: %s", redacted)
	}
}
