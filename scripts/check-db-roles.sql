-- Proves the grants of migration 00013 on a migrated database (make
-- db-check): each role can run what its workload runs, and is denied what
-- it must never do. Runs as the dev superuser, which may SET ROLE to any
-- role; everything is rolled back.
\set ON_ERROR_STOP 1
BEGIN;

-- q must succeed as role r.
CREATE FUNCTION pg_temp.allowed(r text, q text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    EXECUTE format('SET LOCAL ROLE %I', r);
    EXECUTE q;
    RESET ROLE;
END
$$;

-- q must fail as role r with insufficient_privilege.
CREATE FUNCTION pg_temp.denied(r text, q text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        EXECUTE format('SET LOCAL ROLE %I', r);
        EXECUTE q;
    EXCEPTION WHEN insufficient_privilege THEN
        RETURN; -- the subtransaction rollback also undoes SET LOCAL ROLE
    END;
    RAISE EXCEPTION '% may run: %', r, q;
END
$$;

-- commerce-api reads orders and policies, and nothing of the agents'.
SELECT pg_temp.allowed('commerce_ro', 'SELECT * FROM orders LIMIT 1');
SELECT pg_temp.allowed('commerce_ro', 'SELECT * FROM policies LIMIT 1');
SELECT pg_temp.denied('commerce_ro', 'SELECT 1 FROM tickets');
SELECT pg_temp.denied('commerce_ro', 'SELECT 1 FROM run_items');
SELECT pg_temp.denied('commerce_ro', 'DELETE FROM orders WHERE false');
SELECT pg_temp.denied('commerce_ro', 'UPDATE tickets SET status = status WHERE false');

-- The worker: the queue claim, the outbox insert, runs and transcripts.
SELECT pg_temp.allowed('worker_rw', 'SELECT id FROM tickets FOR UPDATE SKIP LOCKED LIMIT 1');
SELECT pg_temp.allowed('worker_rw', 'UPDATE tickets SET status = status WHERE false');
SELECT pg_temp.allowed('worker_rw', 'SELECT id FROM tasks FOR UPDATE SKIP LOCKED LIMIT 1');
SELECT pg_temp.allowed('worker_rw', $q$INSERT INTO runs (agent, model, autonomy, prompt_sha) VALUES ('support', 'fake/x', 'shadow', 'check') RETURNING id$q$);
SELECT pg_temp.allowed('worker_rw', 'UPDATE runs SET status = status WHERE false');
SELECT pg_temp.allowed('worker_rw', 'INSERT INTO tasks (ticket_id, type, reason, order_number, customer_email) SELECT ticket_id, type, reason, order_number, customer_email FROM tasks WHERE false');
SELECT pg_temp.allowed('worker_rw', 'INSERT INTO run_items OVERRIDING SYSTEM VALUE SELECT * FROM run_items WHERE false');
SELECT pg_temp.allowed('worker_rw', 'INSERT INTO run_steps OVERRIDING SYSTEM VALUE SELECT * FROM run_steps WHERE false');
-- Order facts come only through commerce-api.
SELECT pg_temp.denied('worker_rw', 'SELECT 1 FROM orders');
SELECT pg_temp.denied('worker_rw', 'SELECT 1 FROM policies');
SELECT pg_temp.denied('worker_rw', 'DELETE FROM run_items WHERE false');
SELECT pg_temp.denied('worker_rw', 'DELETE FROM tickets WHERE false');
-- Migration 00014: only the queue columns; no DDL, no temp tables.
SELECT pg_temp.denied('worker_rw', 'UPDATE tickets SET sender_verified = true WHERE false');
SELECT pg_temp.denied('worker_rw', 'UPDATE tickets SET customer_email = customer_email WHERE false');
SELECT pg_temp.denied('worker_rw', 'UPDATE tasks SET order_number = order_number WHERE false');
SELECT pg_temp.allowed('worker_rw', 'UPDATE tasks SET status = status, proposal = proposal, last_error = last_error WHERE false');
SELECT pg_temp.denied('worker_rw', 'CREATE TABLE public.check_ddl (i int)');
SELECT pg_temp.denied('worker_rw', 'CREATE TEMP TABLE check_tmp (i int)');

-- The purge job: the retention DELETE, and not one transcript byte.
-- An expired item with a step: the retention DELETE cascades to run_steps
-- with the owner's rights, though purger has no DELETE on run_steps.
WITH r AS (INSERT INTO runs (agent, model, autonomy, prompt_sha) VALUES ('support', 'fake/x', 'shadow', 'check') RETURNING id),
     i AS (INSERT INTO run_items (run_id, subject_kind, subject_id, attempt, outcome, latency_ms, created_at)
           SELECT id, 'ticket', 0, 1, 'abandoned', 0, now() - interval '400 days' FROM r RETURNING id)
INSERT INTO run_steps (run_item_id, seq, kind, name, started_at, latency_ms) SELECT id, 1, 'model', 'fake/x', now(), 0 FROM i;
SELECT pg_temp.allowed('purger', 'DELETE FROM run_items WHERE created_at < now() - make_interval(days => 30)');
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM run_steps s JOIN run_items i ON i.id = s.run_item_id WHERE i.created_at < now() - interval '399 days')
       OR EXISTS (SELECT 1 FROM run_steps s WHERE NOT EXISTS (SELECT 1 FROM run_items i WHERE i.id = s.run_item_id)) THEN
        RAISE EXCEPTION 'purge left run_steps behind';
    END IF;
END $$;
SELECT pg_temp.denied('purger', 'DELETE FROM run_steps WHERE false');
SELECT pg_temp.denied('purger', 'SELECT transcript FROM run_items');
SELECT pg_temp.denied('purger', 'SELECT 1 FROM tickets');
SELECT pg_temp.denied('purger', 'DELETE FROM tickets WHERE false');

ROLLBACK;
\echo 'db roles: grants as intended (commerce_ro, worker_rw, purger)'
