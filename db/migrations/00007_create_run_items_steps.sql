-- What each run did per item and per step: latency, tokens and cost, for
-- dashboards and cost queries (SELECT run_id, sum(cost_micros) ...). Written
-- best-effort after the item's finalize, in their own transaction: losing a
-- row loses telemetry, never a decision.
--
-- Token columns follow agent.Usage: input_tokens is UNCACHED input; cache
-- reads and writes are counted apart. Costs are micro-USD frozen at write
-- time from the registry price, so repricing a model never rewrites history.

-- +goose Up
CREATE TABLE run_items (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id        uuid        NOT NULL REFERENCES runs (id),
    subject_kind  text        NOT NULL CHECK (subject_kind IN ('ticket', 'task')),
    subject_id    bigint      NOT NULL,
    attempt       integer     NOT NULL,
    outcome       text        NOT NULL,  -- final status | 'abandoned' | 'lost_lease'
    source        text,                  -- triage.Source* when decided
    latency_ms    integer     NOT NULL,
    cost_micros   bigint      NOT NULL DEFAULT 0,
    input_tokens  integer,
    output_tokens integer,
    steps         integer,
    error         text,
    transcript    jsonb,                 -- contains customer text: same retention as tickets
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, subject_kind, subject_id, attempt)
);

CREATE TABLE run_steps (
    id                 bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_item_id        bigint      NOT NULL REFERENCES run_items (id) ON DELETE CASCADE,
    seq                integer     NOT NULL,
    kind               text        NOT NULL CHECK (kind IN ('model', 'tool')),
    name               text        NOT NULL,  -- model id or tool name
    started_at         timestamptz NOT NULL,
    latency_ms         integer     NOT NULL,
    input_tokens       integer,
    output_tokens      integer,
    cache_read_tokens  integer,
    cache_write_tokens integer,
    cost_micros        bigint,
    finish_reason      text,
    is_error           boolean     NOT NULL DEFAULT false,
    error              text,
    UNIQUE (run_item_id, seq)
);

-- +goose Down
DROP TABLE run_steps;
DROP TABLE run_items;
