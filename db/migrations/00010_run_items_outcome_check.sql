-- run_items.outcome is a closed list: an item's final status (reported by
-- the handler through obs.ItemRecorder.Decided) or one of the obs.Outcome*
-- values. 'finalized_unknown' flags a handler that finalized without
-- reporting its status: a bug, stored instead of guessed.

-- +goose Up
ALTER TABLE run_items ADD CONSTRAINT run_items_outcome_check CHECK (outcome IN (
    'auto_replied', 'drafted', 'escalated', 'proposed',
    'abandoned', 'lost_lease', 'abandon_failed', 'finalized_unknown'
));

COMMENT ON COLUMN run_items.outcome IS
    'The item''s final status (auto_replied, drafted, escalated for tickets; proposed for tasks) when finalized, '
    'else abandoned | lost_lease | abandon_failed; finalized_unknown when the handler never reported its status.';

-- +goose Down
COMMENT ON COLUMN run_items.outcome IS NULL;
ALTER TABLE run_items DROP CONSTRAINT run_items_outcome_check;
