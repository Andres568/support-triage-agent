package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/Andres568/support-triage-agent/internal/dbtest"
)

// A login user created after migration 00013 gets its group role from
// grantLogins; a missing user is skipped; a second run is a no-op.
func TestGrantLogins_GrantsExistingUsersIdempotently(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	login := fmt.Sprintf("triage_grants_test_%d", os.Getpid()) // roles are cluster-wide
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN", login)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP ROLE IF EXISTS %s", login)) })
	conn := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = conn.Close() })

	pairs := [][2]string{{login, "worker_rw"}, {login + "_missing", "purger"}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for range 2 {
		if err := grantLogins(ctx, conn, pairs, quiet); err != nil {
			t.Fatal(err)
		}
	}
	var member bool
	if err := pool.QueryRow(ctx, `SELECT pg_has_role($1, 'worker_rw', 'MEMBER')`, login).Scan(&member); err != nil {
		t.Fatal(err)
	}
	if !member {
		t.Errorf("%s is not a member of worker_rw", login)
	}
}
