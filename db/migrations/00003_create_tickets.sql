-- The agent's work queue. Each row is a state machine:
--
--   pending → claimed → auto_replied | drafted | escalated | failed
--                ↑  │
--                └──┘ lease expired: claimable again
--
-- Workers claim with FOR UPDATE SKIP LOCKED and finish with a fenced
-- UPDATE (... AND status = 'claimed' AND claimed_by = $run).

-- +goose Up
CREATE TABLE tickets (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    external_id    text        NOT NULL UNIQUE,  -- source system's id; makes ingestion idempotent
    -- Untrusted customer input. No FK to orders: customers mistype order
    -- numbers, and the agent must verify them through get_order anyway.
    order_number   text,
    customer_email text        NOT NULL,
    subject        text        NOT NULL,
    body           text        NOT NULL,

    status         text        NOT NULL DEFAULT 'pending' CHECK (status IN (
                       'pending', 'claimed', 'auto_replied', 'drafted', 'escalated', 'failed'
                   )),
    claimed_by     uuid,                          -- run that owns (or last owned) the ticket
    lease_until    timestamptz,
    attempts       integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    decision       jsonb,                         -- {category, action, draft_reply, confidence, escalate}
    last_error     text,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    -- A claimed ticket always has an owner and a lease.
    CONSTRAINT tickets_claimed_has_lease CHECK (
        status <> 'claimed' OR (claimed_by IS NOT NULL AND lease_until IS NOT NULL)
    ),
    -- Every agent outcome carries its structured decision.
    CONSTRAINT tickets_outcome_has_decision CHECK (
        status NOT IN ('auto_replied', 'drafted', 'escalated') OR decision IS NOT NULL
    )
);

-- Partial indexes stay small: they only cover rows a worker can claim.
CREATE INDEX tickets_pending_idx       ON tickets (created_at)  WHERE status = 'pending';
CREATE INDEX tickets_claimed_lease_idx ON tickets (lease_until) WHERE status = 'claimed';

-- +goose Down
DROP TABLE tickets;
