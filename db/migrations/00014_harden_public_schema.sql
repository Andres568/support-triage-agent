-- Hardens what migration 00013's roles can reach beyond their grants
-- (docs/adr/0015-security-model.md):
--
-- * public schema: owned by the schema owner (the migrating user), not
--   pg_database_owner, and no CREATE for PUBLIC (already the default on
--   PG 15+; revoked again so the intent is explicit and survives a restore
--   from an older dump). The owner change is skipped, with a notice, when
--   the migrating user may not take ownership.
-- * database: PUBLIC keeps CONNECT only (TEMPORARY is revoked).
-- * worker_rw: UPDATE only on the queue columns of tickets and tasks, so a
--   compromised worker cannot rewrite who a ticket is from (sender_verified,
--   customer_email) or what a task is about.
-- * purger: no DELETE on run_steps; the ON DELETE CASCADE from run_items
--   runs with the table owner's rights.
--
-- Not fixed here: on Cloud SQL the built-in users are members of
-- cloudsqlsuperuser (CREATEROLE, CREATEDB); see ADR-0015.

-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
    owner oid := (SELECT nspowner FROM pg_namespace WHERE nspname = 'public');
BEGIN
    IF owner <> (SELECT oid FROM pg_roles WHERE rolname = current_user) THEN
        IF pg_has_role(current_user, owner, 'MEMBER') THEN
            EXECUTE format('ALTER SCHEMA public OWNER TO %I', current_user);
        ELSE
            RAISE NOTICE 'public schema owner % kept: % may not take ownership', owner::regrole, current_user;
        END IF;
    END IF;
    EXECUTE format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO PUBLIC', current_database());
END
$$;
-- +goose StatementEnd
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

REVOKE UPDATE ON tickets, tasks FROM worker_rw;
GRANT UPDATE (status, claimed_by, lease_until, attempts, decision, last_error) ON tickets TO worker_rw;
GRANT UPDATE (status, claimed_by, lease_until, attempts, proposal, last_error) ON tasks TO worker_rw;

REVOKE DELETE ON run_steps FROM purger;

-- +goose Down
GRANT DELETE ON run_steps TO purger;

REVOKE UPDATE (status, claimed_by, lease_until, attempts, decision, last_error) ON tickets FROM worker_rw;
REVOKE UPDATE (status, claimed_by, lease_until, attempts, proposal, last_error) ON tasks FROM worker_rw;
GRANT UPDATE ON tickets, tasks TO worker_rw;

-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO PUBLIC', current_database());
END
$$;
-- +goose StatementEnd
