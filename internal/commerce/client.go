package commerce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// CustomerEmailHeader carries the customer's email on order lookups. A header,
// not a query parameter, so the email never appears in URLs, which end up in
// error messages, proxy logs and traces.
const CustomerEmailHeader = "X-Customer-Email"

// ErrRejected means the API refused the request itself (400, 409 or 422):
// the caller's input is wrong, and retrying it will not help. It is not an
// infrastructure failure. Auth (401, 403), timeouts (408) and throttling
// (429) are: they say nothing about the input, so they are retried.
var ErrRejected = errors.New("commerce api: request rejected")

// Client calls the commerce API over HTTP, as a tool in production would.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{
		Timeout: 10 * time.Second,
		// The API never redirects; following one could carry the customer's
		// email header (and, via IDTokenTransport, a token) somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) Order(ctx context.Context, orderNumber, customerEmail string) (Order, error) {
	var o Order
	u := fmt.Sprintf("%s/v1/orders/%s", c.BaseURL, url.PathEscape(orderNumber))
	return o, c.get(ctx, "get order", u, http.Header{CustomerEmailHeader: {customerEmail}}, &o)
}

func (c *Client) SearchPolicies(ctx context.Context, query string) ([]Policy, error) {
	var body struct {
		Policies []Policy `json:"policies"`
	}
	u := fmt.Sprintf("%s/v1/policies?q=%s", c.BaseURL, url.QueryEscape(query))
	return body.Policies, c.get(ctx, "search policies", u, nil, &body)
}

// get never puts the URL in its errors: they reach the model, logs and
// tickets.last_error, and a URL may carry what the customer typed.
func (c *Client) get(ctx context.Context, op, u string, h http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("commerce api: %s: %w", op, withoutURL(err))
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("commerce api: %s: %w", op, withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }() // read-only body: nothing to do on error

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%w: %s: %s: %s", ErrRejected, op, resp.Status, b)
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("commerce api: %s: %s: %s", op, resp.Status, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("commerce api: %s: decode: %w", op, err)
	}
	return nil
}

// withoutURL unwraps *url.Error, whose message embeds the full URL.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
