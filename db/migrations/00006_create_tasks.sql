-- The handoff between the support agent and the orders agent (an outbox).
-- Rows are inserted in the same transaction as the fenced ticket update, so
-- a task exists if and only if its ticket was finalized. Same state machine
-- as tickets, with one outcome: a proposal for a human.
--
--   pending → claimed → proposed | failed
--
-- Only structured fields cross: a closed type list and a validated order
-- number. Customer text never does, so an injection cannot hop agents.

-- +goose Up
CREATE TABLE tasks (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ticket_id      bigint      NOT NULL REFERENCES tickets (id),
    type           text        NOT NULL CHECK (type IN ('reprint_request', 'address_change', 'refund_review')),
    order_number   text        NOT NULL CHECK (order_number ~ '^ORD-[0-9]{6}$'),
    customer_email text        NOT NULL,          -- copied from the tickets row by SQL, never from the model

    status         text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'claimed', 'proposed', 'failed')),
    claimed_by     uuid        REFERENCES runs (id),
    lease_until    timestamptz,
    attempts       integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    proposal       jsonb,
    last_error     text,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    -- Retries of the same ticket never duplicate a task.
    UNIQUE (ticket_id, type),
    CONSTRAINT tasks_claimed_has_lease CHECK (
        status <> 'claimed' OR (claimed_by IS NOT NULL AND lease_until IS NOT NULL)
    ),
    CONSTRAINT tasks_outcome_has_proposal CHECK (status <> 'proposed' OR proposal IS NOT NULL)
);

CREATE INDEX tasks_pending_idx       ON tasks (created_at)  WHERE status = 'pending';
CREATE INDEX tasks_claimed_lease_idx ON tasks (lease_until) WHERE status = 'claimed';

CREATE TRIGGER tasks_set_updated_at
    BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE tasks;
