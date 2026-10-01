// Package commerce is the internal commerce API the agents' tools call.
// Agents never touch the database directly: this API is the boundary that
// decides what they may see.
package commerce

import (
	"encoding/json"
	"time"
)

// Order is what the API returns to tools. It deliberately omits the customer
// email and address: the model only gets what it needs to answer.
type Order struct {
	OrderNumber string          `json:"order_number"`
	Status      string          `json:"status"`
	Items       json.RawMessage `json:"items"`
	TotalCents  int             `json:"total_cents"`
	Currency    string          `json:"currency"`
	Carrier     string          `json:"carrier,omitempty"`
	Tracking    string          `json:"tracking_number,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	ShippedAt   *time.Time      `json:"shipped_at,omitempty"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
	// Domestic says which shipping-delay limits apply. Not PII: no address.
	Domestic bool `json:"domestic"`

	// Computed here, not by the model: LLMs are unreliable at date math, and
	// policies are written in business days.
	BusinessDaysSinceShipped   *int `json:"business_days_since_shipped,omitempty"`
	BusinessDaysSinceDelivered *int `json:"business_days_since_delivered,omitempty"`
	// Reprint windows are in calendar days ("within 30 days of delivery").
	DaysSinceDelivered *int `json:"days_since_delivered,omitempty"`
}

type Policy struct {
	Slug  string  `json:"slug"`
	Title string  `json:"title"`
	Body  string  `json:"body"`
	Rank  float32 `json:"rank"`
}

// BusinessDaysBetween counts weekdays after from, up to and including to.
// Holidays are ignored: close enough for policy thresholds in a demo.
func BusinessDaysBetween(from, to time.Time) int {
	from, to = dateOnly(from), dateOnly(to)
	n := 0
	for d := from.AddDate(0, 0, 1); !d.After(to); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return n
}

// DaysBetween counts calendar days from from's date to to's date (UTC).
func DaysBetween(from, to time.Time) int {
	return int(dateOnly(to).Sub(dateOnly(from)).Hours() / 24)
}

// internationalCarriers ship abroad. The orders table has no destination
// country (and the address is PII the API never returns), so for the seed
// the carrier decides: Canada Post ships to Canada, every other carrier is
// domestic (US). A real shop would store the destination country instead.
var internationalCarriers = map[string]bool{"Canada Post": true}

// IsDomestic reports whether an order ships within the country.
func IsDomestic(carrier string) bool {
	return !internationalCarriers[carrier]
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// withComputedDays fills the business-day fields relative to now.
func (o *Order) withComputedDays(now time.Time) {
	if o.ShippedAt != nil {
		n := BusinessDaysBetween(*o.ShippedAt, now)
		o.BusinessDaysSinceShipped = &n
	}
	if o.DeliveredAt != nil {
		n := BusinessDaysBetween(*o.DeliveredAt, now)
		d := DaysBetween(*o.DeliveredAt, now)
		o.DaysSinceDelivered = &d
		o.BusinessDaysSinceDelivered = &n
	}
}
