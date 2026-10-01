-- Why a task exists, from a closed list (handoff.Reasons): the support agent
-- read the ticket and its gate checked the reason against the category and
-- the verified order. The orders agent gets the reason instead of any
-- customer text, so it can apply the right policy without judging the claim.
-- Existing rows predate reasons; they get customer_request, the one reason
-- that grants nothing by itself.

-- +goose Up
ALTER TABLE tasks ADD COLUMN reason text NOT NULL DEFAULT 'customer_request'
    CHECK (reason IN ('damaged', 'misprint', 'lost_in_transit', 'cancelled_before_production', 'customer_request'));
ALTER TABLE tasks ALTER COLUMN reason DROP DEFAULT;

-- +goose Down
ALTER TABLE tasks DROP COLUMN reason;
