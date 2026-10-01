package commerce

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBusinessDaysBetween(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	tests := []struct {
		from, to string
		want     int
	}{
		{"2026-09-28", "2026-09-28", 0}, // same day
		{"2026-09-25", "2026-09-28", 1}, // Fri → Mon: only Monday counts
		{"2026-09-21", "2026-09-28", 5}, // Mon → next Mon
		{"2026-09-26", "2026-09-27", 0}, // Sat → Sun
		{"2026-09-01", "2026-09-29", 20},
	}
	for _, tt := range tests {
		if got := BusinessDaysBetween(day(tt.from), day(tt.to)); got != tt.want {
			t.Errorf("BusinessDaysBetween(%s, %s) = %d, want %d", tt.from, tt.to, got, tt.want)
		}
	}
}

// seededAPI serves the real handler over the real store, backed by a database
// loaded with db/seed/seed.sql. `make db-check` provides one; plain
// `go test` skips these tests.
func seededAPI(t *testing.T) *Client {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set (run via make db-check)")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	srv := httptest.NewServer(NewHandler(NewStore(pool), slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

func TestAPI_OrderOnlyForOwner(t *testing.T) {
	c := seededAPI(t)
	ctx := context.Background()

	o, err := c.Order(ctx, "ORD-100103", "Carol@Example.com") // email match is case-insensitive
	if err != nil {
		t.Fatalf("owner lookup: %v", err)
	}
	if o.Status != "shipped" || o.Carrier != "USPS" || o.BusinessDaysSinceShipped == nil || !o.Domestic {
		t.Errorf("order = %+v", o)
	}
	if o, err := c.Order(ctx, "ORD-100114", "nina@example.com"); err != nil || o.Domestic {
		t.Errorf("Canada Post order = %+v, %v; want international", o, err)
	}
	if o.BusinessDaysSinceDelivered != nil {
		t.Error("undelivered order has business_days_since_delivered")
	}

	// Another customer, and a nonexistent order, get the same answer.
	for _, tc := range []struct{ number, email string }{
		{"ORD-100103", "quinn@example.com"},
		{"ORD-999999", "carol@example.com"},
	} {
		if _, err := c.Order(ctx, tc.number, tc.email); err != ErrNotFound {
			t.Errorf("Order(%s, %s) err = %v, want ErrNotFound", tc.number, tc.email, err)
		}
	}
}

func TestAPI_OrderResponseHidesCustomerEmail(t *testing.T) {
	c := seededAPI(t)
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/v1/orders/ORD-100103", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(CustomerEmailHeader, "carol@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only body: nothing to do on error
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["customer_email"]; ok {
		t.Error("response exposes customer_email")
	}
}

func TestAPI_RequiresCustomerEmail(t *testing.T) {
	c := seededAPI(t)
	resp, err := http.Get(c.BaseURL + "/v1/orders/ORD-100103")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// Client errors reach the model, logs and tickets.last_error: they must
// never carry the customer's email, whatever failed.
func TestClient_ErrorsNeverContainTheEmail(t *testing.T) {
	const email = "secret.person@example.com"
	statuses := map[string]int{
		"rejected": http.StatusBadRequest, "unprocessable": http.StatusUnprocessableEntity, "server": http.StatusBadGateway,
		"throttled": http.StatusTooManyRequests, "unauthorized": http.StatusUnauthorized, "forbidden": http.StatusForbidden,
	}
	for name, status := range statuses {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", status)
		}))
		_, err := NewClient(srv.URL).Order(context.Background(), "ORD-100103", email)
		srv.Close()
		if err == nil || strings.Contains(err.Error(), email) {
			t.Errorf("%s: err = %v, want an error without the email", name, err)
		}
		if got := errors.Is(err, ErrRejected); got != (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) {
			t.Errorf("%s: errors.Is(err, ErrRejected) = %v", name, got)
		}
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused: a transport error, i.e. a *url.Error
	_, err := NewClient(srv.URL).Order(context.Background(), "ORD-100103", email)
	if err == nil || strings.Contains(err.Error(), email) || strings.Contains(err.Error(), "ORD-100103") {
		t.Errorf("transport err = %v, want an error without the URL", err)
	}
}

func TestAPI_SearchPolicies(t *testing.T) {
	c := seededAPI(t)
	tests := map[string]string{ // query → expected top policy
		"mugs cracked damaged": "reprint-damaged", // AND semantics found nothing here
		"chargeback":           "disputes-and-legal",
		"package not arrived":  "shipping-delay",
		"cancel my order":      "cancellations-and-changes",
		"microwave safe":       "materials-and-care",
	}
	for q, want := range tests {
		ps, err := c.SearchPolicies(context.Background(), q)
		if err != nil {
			t.Fatalf("SearchPolicies(%q): %v", q, err)
		}
		if len(ps) == 0 || ps[0].Slug != want {
			t.Errorf("SearchPolicies(%q) top = %v, want %s", q, slugs(ps), want)
		}
		if len(ps) > 3 {
			t.Errorf("SearchPolicies(%q) returned %d, want at most 3", q, len(ps))
		}
	}
}

func slugs(ps []Policy) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Slug
	}
	return out
}

func TestClient_DoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer target.Close()
	api := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer api.Close()
	if _, err := NewClient(api.URL).Order(context.Background(), "ORD-100101", "a@example.com"); err == nil {
		t.Error("a redirect was treated as success")
	}
	if followed.Load() {
		t.Error("client followed the redirect")
	}
}

// panicQuerier fails the test if a request reaches the database.
type panicQuerier struct{ t *testing.T }

func (p panicQuerier) OrderForCustomer(context.Context, string, string) (Order, error) {
	p.t.Error("invalid order number reached the store")
	return Order{}, nil
}

func (p panicQuerier) SearchPolicies(context.Context, string, int) ([]Policy, error) {
	p.t.Error("invalid query reached the store")
	return nil, nil
}

func TestAPI_RejectsBadInputBeforeTheStore(t *testing.T) {
	srv := httptest.NewServer(NewHandler(panicQuerier{t}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()
	for _, path := range []string{
		"/v1/orders/ord-100101", "/v1/orders/ORD-1001011", "/v1/orders/SM%20100101",
		"/v1/policies?q=" + strings.Repeat("a", maxQueryBytes+1),
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set(CustomerEmailHeader, "a@example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path[:min(len(path), 40)], resp.StatusCode)
		}
	}
}
