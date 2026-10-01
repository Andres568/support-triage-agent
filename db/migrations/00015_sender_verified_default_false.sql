-- sender_verified fails closed: a ticket inserted without it is unverified,
-- so the support gate forces a human draft with no follow-ups. Intake (and
-- the seed) must set true explicitly after a DMARC pass or a logged-in form
-- (migration 00012, CWE-290). Existing rows keep their value.

-- +goose Up
ALTER TABLE tickets ALTER COLUMN sender_verified SET DEFAULT false;

-- +goose Down
ALTER TABLE tickets ALTER COLUMN sender_verified SET DEFAULT true;
