//go:build !js

package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"autivashop/backend/internal/database"

	"github.com/jackc/pgx/v5"
)

// TestPostgreSQLIntegration is opt-in: set TEST_DATABASE_URL to a dedicated
// test database. It never drops, truncates or overwrites any database.
func TestPostgreSQLIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL integration tests skipped")
	}

	cfg := testConfig(t, map[string]string{"DATABASE_URL": dsn, "DB_QUERY_TIMEOUT": "5s"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		t.Skipf("dedicated PostgreSQL test database is unreachable: %v", err)
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping() failed: %v", err)
	}

	if err := db.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	}); err != nil {
		t.Fatalf("WithTx commit path failed: %v", err)
	}

	sentinel := errors.New("intentional rollback")
	if err := db.WithTx(ctx, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, `SELECT 1`)
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("WithTx rollback path returned %v, want sentinel", err)
	}

	migrationsDir := filepath.Join("..", "migrations")
	if _, err := db.Migrate(ctx, migrationsDir); err != nil {
		t.Fatalf("Migrate() failed: %v", err)
	}
	versions, err := db.AppliedVersions(ctx)
	if err != nil {
		t.Fatalf("AppliedVersions() failed: %v", err)
	}
	if !slices.Contains(versions, "0001_foundation.sql") {
		t.Fatalf("0001 migration was not recorded: %v", versions)
	}
	if !slices.Contains(versions, "0002_admin_auth.sql") {
		t.Fatalf("0002 migration was not recorded: %v", versions)
	}

	appliedAgain, err := db.Migrate(ctx, migrationsDir)
	if err != nil {
		t.Fatalf("second Migrate() failed: %v", err)
	}
	if len(appliedAgain) != 0 {
		t.Fatalf("second Migrate() reapplied versions: %v", appliedAgain)
	}
}
