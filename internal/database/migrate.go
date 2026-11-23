package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryer is implemented by both *pgxpool.Pool and *pgxpool.Conn.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// migrationLockID is a fixed application-specific advisory lock key.
const migrationLockID int64 = 0x41555449564153

var migrationNamePattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_.-]*\.sql$`)

// Migrate applies all *.sql files in dir in lexical order, once each.
// It is forward-only and transactional per file. No down migrations exist.
func (d *DB) Migrate(ctx context.Context, dir string) ([]string, error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not acquire migration connection")
	}
	defer conn.Release()

	lockCtx, cancelLock := d.withTimeout(ctx)
	if _, err := conn.Exec(lockCtx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		cancelLock()
		return nil, fmt.Errorf("could not acquire migration lock")
	}
	cancelLock()
	defer func() {
		unlockCtx, cancelUnlock := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelUnlock()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	setupCtx, cancelSetup := d.withTimeout(ctx)
	_, err = conn.Exec(setupCtx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	cancelSetup()
	if err != nil {
		return nil, fmt.Errorf("could not initialize migration tracking table")
	}

	applied, err := d.appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, fmt.Errorf("could not list migration files")
	}
	sort.Strings(files)

	newlyApplied := make([]string, 0)
	for _, file := range files {
		version := filepath.Base(file)
		if !migrationNamePattern.MatchString(version) {
			return nil, fmt.Errorf("migration file name is not allowed")
		}
		if _, exists := applied[version]; exists {
			continue
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("could not read migration file")
		}

		txCtx, cancelTx := d.withTimeout(ctx)
		tx, err := conn.Begin(txCtx)
		if err != nil {
			cancelTx()
			return nil, fmt.Errorf("could not begin migration transaction")
		}
		_, err = tx.Exec(txCtx, string(contents))
		if err == nil {
			_, err = tx.Exec(txCtx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
		}
		if err == nil {
			err = tx.Commit(txCtx)
		} else {
			rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 5*time.Second)
			_ = tx.Rollback(rollbackCtx)
			cancelRollback()
		}
		cancelTx()
		if err != nil {
			return nil, fmt.Errorf("migration %s could not be applied", version)
		}
		applied[version] = struct{}{}
		newlyApplied = append(newlyApplied, version)
	}
	return newlyApplied, nil
}

// AppliedVersions lists all recorded migration versions.
func (d *DB) AppliedVersions(ctx context.Context) ([]string, error) {
	versions, err := d.appliedVersions(ctx, d.pool)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(versions))
	for version := range versions {
		result = append(result, version)
	}
	sort.Strings(result)
	return result, nil
}

func (d *DB) appliedVersions(ctx context.Context, q queryer) (map[string]struct{}, error) {
	queryCtx, cancel := d.withTimeout(ctx)
	defer cancel()
	rows, err := q.Query(queryCtx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("could not read migration versions")
	}
	defer rows.Close()

	versions := make(map[string]struct{})
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("could not read migration version")
		}
		versions[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("could not read migration versions")
	}
	return versions, nil
}
