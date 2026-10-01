-- Business data owned by commerce-api. The agent never reads this table
-- directly; it goes through the API's get_order tool.

-- +goose Up
CREATE TABLE orders (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_number    text        NOT NULL UNIQUE,          -- customer-facing, e.g. 'ORD-100234'
    customer_email  text        NOT NULL,
    status          text        NOT NULL CHECK (status IN (
                        'paid', 'in_production', 'shipped', 'delivered', 'cancelled', 'refunded'
                    )),
    items           jsonb       NOT NULL DEFAULT '[]',    -- [{sku, name, qty, unit_price_cents}]
    total_cents     integer     NOT NULL CHECK (total_cents >= 0),
    currency        char(3)     NOT NULL DEFAULT 'USD',
    carrier         text,
    tracking_number text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    shipped_at      timestamptz,
    delivered_at    timestamptz
);

CREATE INDEX orders_customer_email_idx ON orders (customer_email);

-- +goose Down
DROP TABLE orders;
