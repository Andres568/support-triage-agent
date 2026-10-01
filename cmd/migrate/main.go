// Command migrate applies the embedded migrations (`migrate up`, `migrate
// status`, `migrate grants`) and enforces transcript retention (`migrate purge -days N`, the
// same DELETE as `make purge-runs`; docs/adr/0014). It is the prod migrate
// and purge job (Cloud Run Jobs, or the compose `migrate` service); dev keeps
// the goose CLI (make migrate). Both record versions in the same
// goose_db_version table.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/Andres568/support-triage-agent/db"
	"github.com/Andres568/support-triage-agent/internal/obs"
)

// defaultRetentionDays matches RETENTION_DAYS in the Makefile.
const defaultRetentionDays = 30

// purgeSQL is the DELETE of `make purge-runs`; run_steps go with their item
// (ON DELETE CASCADE). The purger role may run it but cannot read transcripts.
const purgeSQL = `DELETE FROM run_items WHERE created_at < now() - make_interval(days => $1)`

func main() {
	log := obs.NewLogger(os.Stdout, os.Getenv("GOOGLE_CLOUD_PROJECT"))
	command, args := "up", []string(nil)
	if len(os.Args) > 1 {
		command, args = os.Args[1], os.Args[2:]
	}
	if err := run(command, args, os.Getenv("DATABASE_URL"), log); err != nil {
		log.Error("migrate failed", "command", command, "err", err)
		os.Exit(1)
	}
}

func run(command string, args []string, databaseURL string, log *slog.Logger) error {
	if command != "up" && command != "status" && command != "purge" && command != "grants" {
		return fmt.Errorf("unknown command %q: want up, status, purge or grants", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	days := flags.Int("days", defaultRetentionDays, "purge: delete run_items older than this many days")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *days < 1 {
		return fmt.Errorf("-days must be >= 1, got %d", *days)
	}
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	// SIGTERM (Cloud Run task timeout) cancels; each migration runs in its own tx.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if command == "purge" {
		return purge(ctx, conn, *days, log)
	}
	if command == "grants" {
		return grantLogins(ctx, conn, loginGroups, log)
	}
	p, err := newProvider(conn)
	if err != nil {
		return err
	}

	if command == "status" {
		statuses, err := p.Status(ctx)
		for _, s := range statuses {
			log.Info("migration", "version", s.Source.Version, "file", s.Source.Path, "state", string(s.State))
		}
		return err
	}
	results, err := p.Up(ctx)
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration_ms", r.Duration.Milliseconds())
	}
	if err != nil {
		return err
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	log.Info("migrations up to date", "applied", len(results), "version", v)
	return grantLogins(ctx, conn, loginGroups, log)
}

func purge(ctx context.Context, conn *sql.DB, days int, log *slog.Logger) error {
	res, err := conn.ExecContext(ctx, purgeSQL, days)
	if err != nil {
		return fmt.Errorf("purge run_items: %w", err)
	}
	n, err := res.RowsAffected()
	log.Info("run_items purged", "retention_days", days, "deleted", n)
	return err
}

func newProvider(conn *sql.DB) (*goose.Provider, error) {
	migrations, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return nil, err
	}
	// A Postgres advisory lock: two migrate executions (a retried deploy, a
	// manual run) apply migrations one at a time instead of racing.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, conn, migrations, goose.WithSessionLocker(locker))
}

// loginGroups maps each login user Terraform creates (infra/terraform/sql.tf)
// to the group role of migration 00013.
var loginGroups = [][2]string{
	{"triage_commerce", "commerce_ro"},
	{"triage_worker", "worker_rw"},
	{"triage_purger", "purger"},
}

// grantLogins grants each existing login user its group role. Migration
// 00013 does the same, but only once: a user created after it ran (a new
// environment, a recreated user) would stay without grants. `up` runs this
// every time, so the grants follow the users. Idempotent; a missing user is
// skipped (locally everything connects as the dev superuser).
func grantLogins(ctx context.Context, conn *sql.DB, pairs [][2]string, log *slog.Logger) error {
	for _, p := range pairs {
		login, group := p[0], p[1]
		var exists bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, login).Scan(&exists); err != nil {
			return fmt.Errorf("grants: %w", err)
		}
		if !exists {
			continue
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("GRANT %s TO %s", pgx.Identifier{group}.Sanitize(), pgx.Identifier{login}.Sanitize())); err != nil {
			return fmt.Errorf("grants: %s to %s: %w", group, login, err)
		}
		log.Info("role granted", "role", group, "login", login)
	}
	return nil
}
