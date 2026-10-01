package commerce

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound covers both "no such order" and "not your order": callers must
// not be able to tell them apart (see policy "order-privacy").
var ErrNotFound = errors.New("not found")

type Store struct {
	db  *pgxpool.Pool
	now func() time.Time
}

func NewStore(db *pgxpool.Pool) *Store {
	return NewStoreWithClock(db, time.Now)
}

// NewStoreWithClock is NewStore with a clock for the computed days, so an
// eval replay sees the days of its recording.
func NewStoreWithClock(db *pgxpool.Pool, now func() time.Time) *Store {
	return &Store{db: db, now: now}
}

// OrderForCustomer returns the order only if it belongs to customerEmail.
// The ownership check is part of the query, so there is no code path that
// loads someone else's order and then forgets to check.
func (s *Store) OrderForCustomer(ctx context.Context, orderNumber, customerEmail string) (Order, error) {
	const q = `
		SELECT order_number, status, items, total_cents, currency,
		       coalesce(carrier, ''), coalesce(tracking_number, ''),
		       created_at, shipped_at, delivered_at
		FROM orders
		WHERE order_number = $1 AND lower(customer_email) = lower($2)`
	var o Order
	err := s.db.QueryRow(ctx, q, strings.TrimSpace(orderNumber), strings.TrimSpace(customerEmail)).Scan(
		&o.OrderNumber, &o.Status, &o.Items, &o.TotalCents, &o.Currency,
		&o.Carrier, &o.Tracking, &o.CreatedAt, &o.ShippedAt, &o.DeliveredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("query order: %w", err)
	}
	o.Domestic = IsDomestic(o.Carrier)
	o.withComputedDays(s.now())
	return o, nil
}

// SearchPolicies ranks policies by full-text match. Terms are OR-ed, not
// AND-ed: a customer's wording ("mugs cracked damaged") rarely matches every
// word of a policy, and ranking already puts the best matches first.
func (s *Store) SearchPolicies(ctx context.Context, query string, limit int) ([]Policy, error) {
	const q = `
		WITH q AS (
		    SELECT to_tsquery('english', replace(plainto_tsquery('english', $1)::text, ' & ', ' | ')) AS query
		)
		SELECT slug, title, body, ts_rank(search, q.query) AS rank
		FROM policies, q
		WHERE search @@ q.query
		ORDER BY rank DESC, slug
		LIMIT $2`
	rows, err := s.db.Query(ctx, q, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search policies: %w", err)
	}
	policies, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Policy])
	if err != nil {
		return nil, fmt.Errorf("search policies: %w", err)
	}
	return policies, nil
}
