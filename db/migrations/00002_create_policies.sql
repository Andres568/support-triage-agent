-- Support policies (refunds, reprints, shipping delays...). Served to the
-- agent by commerce-api's search_policy tool via Postgres full-text search.

-- +goose Up
CREATE TABLE policies (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug       text        NOT NULL UNIQUE,               -- stable key, e.g. 'reprint-damaged'
    title      text        NOT NULL,
    body       text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Kept in sync by Postgres itself; title matches rank above body matches.
    search     tsvector GENERATED ALWAYS AS (
                   setweight(to_tsvector('english', title), 'A') ||
                   setweight(to_tsvector('english', body),  'B')
               ) STORED
);

CREATE INDEX policies_search_idx ON policies USING gin (search);

-- +goose Down
DROP TABLE policies;
