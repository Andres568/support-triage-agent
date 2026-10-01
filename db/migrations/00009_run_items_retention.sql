-- run_items hardening after review (docs/adr/0014-run-transcript-retention.md):
-- what the transcript really holds, a size cap on it, cache token totals,
-- a lookup index by subject, and sanity checks.

-- +goose Up
ALTER TABLE run_items
    ADD COLUMN cache_read_tokens    integer,
    ADD COLUMN cache_write_tokens   integer,
    -- The transcript was over obs.MaxTranscriptBytes and is not stored.
    ADD COLUMN transcript_truncated boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT run_items_attempt_check CHECK (attempt >= 1),
    ADD CONSTRAINT run_items_latency_ms_check CHECK (latency_ms >= 0);

-- "Every attempt at ticket 42": the unique key starts with run_id, so it
-- cannot serve this.
CREATE INDEX run_items_subject_idx ON run_items (subject_kind, subject_id);

COMMENT ON COLUMN run_items.transcript IS
    'The attempt''s whole model conversation: the ticket text (customer PII) AND every tool output, '
    'i.e. commerce order details (addresses, items, totals). NULL when transcript_truncated. '
    'Deleted with the row after RETENTION_DAYS by make purge-runs.';
COMMENT ON COLUMN run_items.cost_micros IS
    'Source of truth for cost: micro-USD frozen at write time, rounded once from the item''s summed usage. '
    'run_steps.cost_micros is per step and rounded per step, so its sum may differ.';

-- +goose Down
COMMENT ON COLUMN run_items.cost_micros IS NULL;
COMMENT ON COLUMN run_items.transcript IS NULL;
DROP INDEX run_items_subject_idx;
ALTER TABLE run_items
    DROP CONSTRAINT run_items_latency_ms_check,
    DROP CONSTRAINT run_items_attempt_check,
    DROP COLUMN transcript_truncated,
    DROP COLUMN cache_write_tokens,
    DROP COLUMN cache_read_tokens;
