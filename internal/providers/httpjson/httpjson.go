// Package httpjson posts JSON to model provider APIs with the few retries
// that are worth doing: rate limits and overloaded or failing servers.
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	// MaxRetries after the first attempt. More would only hide an outage
	// from the queue, which retries the whole item later anyway.
	MaxRetries = 2
	// MaxRetryWait caps a retry-after: a longer wait is better spent on
	// other items, so we give up and let the queue retry.
	MaxRetryWait = 20 * time.Second
	// maxBody bounds what we read from a response: model replies are small.
	maxBody = 8 << 20
)

// baseBackoff is the first wait when the server sends no retry-after; it
// doubles per retry. A variable so tests do not sleep.
var baseBackoff = time.Second

// HTTPError is a non-2xx response. Type and Message come from the provider's
// error body when it has one ({error:{type,message}} in both the Anthropic
// and OpenAI shapes, or {error:"..."}). Code is OpenAI's machine-readable
// error.code (e.g. "context_length_exceeded"), when present. Message is
// truncated, and withheld on 401/403.
type HTTPError struct {
	Status     int
	Type       string
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("http %d", e.Status)
	if e.Type != "" {
		msg += " " + e.Type
	}
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Retryable reports whether the same request may succeed later: rate
// limits (429), server errors (500, 502, 503) and Anthropic's overload (529).
func (e *HTTPError) Retryable() bool {
	switch e.Status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, 529:
		return true
	}
	return false
}

// Post sends body as JSON and decodes a 2xx reply into out. Retryable
// statuses are retried up to MaxRetries times, honoring retry-after (capped
// at MaxRetryWait) and never waiting past ctx. Transport errors are not
// retried here: they are usually the deadline, and the queue retries items.
func Post(ctx context.Context, hc *http.Client, url string, headers http.Header, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("httpjson: encode request: %w", err)
	}
	for attempt := 0; ; attempt++ {
		herr, err := post(ctx, hc, url, headers, payload, out)
		if err != nil {
			return err
		}
		if herr == nil {
			return nil
		}
		if !herr.Retryable() || attempt == MaxRetries {
			return herr
		}
		wait := herr.RetryAfter
		if wait == 0 {
			wait = baseBackoff << attempt
		}
		if wait > MaxRetryWait {
			return herr
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(herr, ctx.Err())
		case <-t.C:
		}
	}
}

// post makes one attempt. It returns an *HTTPError for a non-2xx status and
// a plain error for everything else.
func post(ctx context.Context, hc *http.Client, url string, headers http.Header, payload []byte, out any) (*HTTPError, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("httpjson: %w", err)
	}
	for k, vs := range headers {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpjson: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only body: nothing to do on error
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("httpjson: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(resp.StatusCode, resp.Header.Get("Retry-After"), raw), nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("httpjson: decode response: %w", err)
	}
	return nil, nil
}

func parseError(status int, retryAfter string, raw []byte) *HTTPError {
	e := &HTTPError{Status: status, RetryAfter: parseRetryAfter(retryAfter)}
	defer func() {
		// An auth error may echo the (masked) key; keep it out of our logs.
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			e.Message = authMessage
		}
		e.Message = Truncate(e.Message)
	}()
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Error) == 0 {
		e.Message = string(raw)
		return e
	}
	var obj struct {
		Type, Message string
		Code          json.RawMessage // a string, null, or absent depending on the provider
	}
	if json.Unmarshal(body.Error, &obj) == nil {
		e.Type, e.Message = obj.Type, obj.Message
		_ = json.Unmarshal(obj.Code, &e.Code) // not a string: no code
		return e
	}
	var s string
	if json.Unmarshal(body.Error, &s) == nil {
		e.Message = s
	}
	return e
}

// authMessage replaces the provider's message on 401 and 403.
const authMessage = "authentication or permission error (provider message withheld)"

// parseRetryAfter reads seconds or an HTTP date; anything else is "unset".
// Values beyond maxRetryAfter are clamped before converting, so a huge or
// non-finite header cannot overflow time.Duration; Post then gives up on it.
func parseRetryAfter(v string) time.Duration {
	const maxRetryAfter = time.Hour
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		switch {
		case math.IsNaN(s) || s < 0:
			return 0
		case s > maxRetryAfter.Seconds(): // also +Inf
			return maxRetryAfter
		}
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxRetryAfter)
	}
	return 0
}

// Truncate bounds a provider message on a rune boundary, so a cut never
// leaves invalid UTF-8 in logs or the database.
func Truncate(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
