-- Keep tickets.updated_at honest: set it on every UPDATE instead of relying
-- on each query to remember. Mechanical bookkeeping only; state-transition
-- rules stay in application code.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER tickets_set_updated_at
    BEFORE UPDATE ON tickets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TRIGGER tickets_set_updated_at ON tickets;
DROP FUNCTION set_updated_at();
