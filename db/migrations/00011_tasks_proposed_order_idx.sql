-- The orders agent's duplicate check (orders.TaskStore.EligibleDuplicate)
-- looks up proposed tasks of one order before every eligible proposal.

-- +goose Up
CREATE INDEX tasks_proposed_order_idx ON tasks (order_number, type) WHERE status = 'proposed';

-- +goose Down
DROP INDEX tasks_proposed_order_idx;
