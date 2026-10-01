-- One row per worker execution (a batch of one agent). Its id is the lease
-- owner on tickets and tasks, so every claim can be traced back to the run,
-- model, autonomy level and prompt version that made it.

-- +goose Up
CREATE TABLE runs (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent       text        NOT NULL CHECK (agent IN ('support', 'orders')),
    model       text        NOT NULL,                 -- registry id, e.g. 'ollama/qwen3:8b'
    autonomy    text        NOT NULL CHECK (autonomy IN ('shadow', 'suggest', 'auto')),
    prompt_sha  text        NOT NULL,
    status      text        NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'succeeded', 'failed')),
    claimed     integer     NOT NULL DEFAULT 0,
    finalized   integer     NOT NULL DEFAULT 0,
    abandoned   integer     NOT NULL DEFAULT 0,
    lost_leases integer     NOT NULL DEFAULT 0,
    error       text,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

-- A lease owner must be a real run, not an arbitrary uuid.
ALTER TABLE tickets ADD CONSTRAINT tickets_claimed_by_fk FOREIGN KEY (claimed_by) REFERENCES runs (id);

-- 00003 described an older decision shape in a SQL comment; this is the
-- current contract, stored where tools like psql \d+ can see it.
COMMENT ON COLUMN tickets.decision IS 'triage.Record: {source, proposed, final, overrides, autonomy, model}';

-- +goose Down
COMMENT ON COLUMN tickets.decision IS NULL;
ALTER TABLE tickets DROP CONSTRAINT tickets_claimed_by_fk;
DROP TABLE runs;
