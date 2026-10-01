-- Bounds a ticket's size at the boundary: every byte is resent to the model
-- at every step, so an unbounded body is an unbounded bill (CWE-400). 32 KiB
-- is far above any real support email's text.
--
-- sender_verified records whether the sender proved they own customer_email.
-- get_order trusts that address, so intake must set it to true only after a
-- DMARC pass on the inbound mail or a logged-in form (CWE-290); anything
-- else must insert false, and the support gate then forces a human draft
-- with no follow-ups. The default is true only because the seed and the
-- demo intake are trusted; a real intake should set it explicitly.

-- +goose Up
ALTER TABLE tickets
    ADD CONSTRAINT tickets_size_check CHECK (octet_length(body) <= 32768 AND octet_length(subject) <= 1024),
    ADD COLUMN sender_verified boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE tickets
    DROP COLUMN sender_verified,
    DROP CONSTRAINT tickets_size_check;
