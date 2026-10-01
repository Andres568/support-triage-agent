// Package dbtest gives each DB-backed test its own database, cloned from a
// migrated and seeded template. Tests can then mutate freely and run in
// parallel without seeing each other's rows.
package dbtest

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fresh returns a pool on a new database cloned from $TEST_TEMPLATE_DB and
// drops it when the test ends. $TEST_DATABASE_URL is the admin connection
// used to create and drop it. `make db-check` sets both; plain `go test`
// skips.
func Fresh(t *testing.T) *pgxpool.Pool {
	t.Helper()
	template, adminURL := os.Getenv("TEST_TEMPLATE_DB"), os.Getenv("TEST_DATABASE_URL")
	if template == "" || adminURL == "" {
		t.Skip("TEST_TEMPLATE_DB or TEST_DATABASE_URL not set (run via make db-check)")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	// Creation time in the name lets scripts/drop-stale-test-dbs.sql clean up
	// after a crashed run without touching a concurrent one.
	name := fmt.Sprintf("triage_t_%d_%s", time.Now().Unix(), strings.ToLower(rand.Text()))
	create := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " TEMPLATE " + pgx.Identifier{template}.Sanitize()
	if _, err := admin.Exec(ctx, create); err != nil {
		t.Fatalf("clone template: %v", err)
	}
	// Registered before the pool's Close, so it runs after it (LIFO).
	t.Cleanup(func() {
		drop := "DROP DATABASE " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
		if _, err := admin.Exec(context.Background(), drop); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
