-- Least-privilege database roles (docs/adr/0015-security-model.md). NOLOGIN
-- group roles hold the grants; login users are granted into them:
--
--   commerce_ro  commerce-api: reads orders and policies, nothing else.
--   worker_rw    both agents: the queues (tickets, tasks), runs and
--                transcripts. No access to orders or policies: order facts
--                reach the model only through commerce-api.
--   purger       the retention job: deletes old run_items (steps cascade);
--                sees only created_at, never a transcript.
--
-- Migrations keep running as the schema owner (triage in prod, the dev
-- superuser locally); ownership already grants DDL, so no migrator group
-- role is needed. Every new table must GRANT to these roles in its own
-- migration (no default privileges: a grant is a decision).
--
-- Identity columns need no sequence grant: Postgres does not check sequence
-- privileges for GENERATED ... AS IDENTITY (scripts/check-db-roles.sql
-- proves the inserts work).
--
-- Roles are cluster-wide, and dev, check and eval databases share a
-- cluster, so creation is idempotent and Down drops a role only when no
-- other database still grants it anything.

-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['commerce_ro', 'worker_rw', 'purger'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN', r);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT SELECT ON orders, policies TO commerce_ro;

GRANT SELECT, UPDATE ON tickets TO worker_rw;
GRANT SELECT, INSERT, UPDATE ON tasks, runs TO worker_rw;
GRANT SELECT, INSERT ON run_items, run_steps TO worker_rw;

GRANT SELECT (created_at) ON run_items TO purger;
GRANT DELETE ON run_items, run_steps TO purger;

-- Login users exist only where Terraform created them (infra/terraform/sql.tf);
-- locally everything connects as the dev superuser.
-- +goose StatementBegin
DO $$
DECLARE
    m record;
BEGIN
    FOR m IN SELECT * FROM (VALUES ('triage_commerce', 'commerce_ro'), ('triage_worker', 'worker_rw'), ('triage_purger', 'purger')) AS v (login, grp) LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = m.login) THEN
            EXECUTE format('GRANT %I TO %I', m.grp, m.login);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
REVOKE ALL ON orders, policies, tickets, tasks, runs, run_items, run_steps FROM commerce_ro, worker_rw, purger;
REVOKE SELECT (created_at) ON run_items FROM purger;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['commerce_ro', 'worker_rw', 'purger'] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_shdepend d JOIN pg_roles o ON o.oid = d.refobjid
            WHERE d.refclassid = 'pg_authid'::regclass AND o.rolname = r
        ) THEN
            EXECUTE format('DROP ROLE IF EXISTS %I', r);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
