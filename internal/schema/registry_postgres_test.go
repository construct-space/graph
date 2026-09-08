package schema

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"construct-graph/internal/engine"
)

// requirePostgres skips the test when TEST_PG_DSN isn't set so unit-test
// machines without a Postgres instance keep passing.
func requirePostgres(t *testing.T) *Registry {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set; skipping Postgres integration test")
	}
	db, err := engine.Connect(dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	r := NewRegistry(db)
	if err := r.InitSystem(); err != nil {
		t.Fatalf("init system: %v", err)
	}
	return r
}

// TestCreateSchemaIfNotExistsSafe_Concurrent reproduces the production race
// where two simultaneous CREATE SCHEMA IF NOT EXISTS calls collide on
// pg_namespace_nspname_index. The advisory-lock fix in
// createSchemaIfNotExistsSafe should serialize them, so all goroutines
// succeed and the schema exists exactly once.
func TestCreateSchemaIfNotExistsSafe_Concurrent(t *testing.T) {
	r := requirePostgres(t)

	if !engine.IsPostgres() {
		t.Skip("not running against postgres")
	}

	schema := "test_concurrent_provision"
	defer func() {
		// Best-effort cleanup so re-runs are idempotent.
		_ = r.db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema)).Error
	}()

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := r.createSchemaIfNotExistsSafe(schema); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent create returned error: %v", err)
	}

	// Confirm the schema exists exactly once.
	var count int64
	if err := r.db.Raw(
		"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?",
		schema,
	).Scan(&count).Error; err != nil {
		t.Fatalf("count schema: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 schema, got %d", count)
	}
}
