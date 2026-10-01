package httpjson

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func init() { baseBackoff = time.Millisecond }

// server replies with statuses in order (the last one repeats) and counts hits.
func server(t *testing.T, retryAfter string, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1))
		status := statuses[min(n, len(statuses))-1]
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Test") != "yes" {
			t.Errorf("headers = %v", r.Header)
		}
		if status != http.StatusOK {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func postOK(t *testing.T, url string) error {
	var out struct{ OK bool }
	err := Post(context.Background(), http.DefaultClient, url, http.Header{"X-Test": {"yes"}}, map[string]int{"a": 1}, &out)
	if err == nil && !out.OK {
		t.Error("response not decoded")
	}
	return err
}

func TestPost(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		statuses   []int
		wantHits   int32
		wantStatus int // 0: success
	}{
		{"ok", "", []int{200}, 1, 0},
		{"429 with retry-after then ok", "0", []int{429, 200}, 2, 0},
		{"529 overload retried", "", []int{529, 503, 200}, 3, 0},
		{"gives up after two retries", "0", []int{500}, 3, 500},
		{"400 is not retried", "", []int{400}, 1, 400},
		{"retry-after beyond the cap is not waited for", "3600", []int{429}, 1, 429},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hits := server(t, tt.retryAfter, tt.statuses...)
			err := postOK(t, srv.URL)
			if hits.Load() != tt.wantHits {
				t.Errorf("hits = %d, want %d", hits.Load(), tt.wantHits)
			}
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var herr *HTTPError
			if !errors.As(err, &herr) || herr.Status != tt.wantStatus || herr.Type != "rate_limit_error" || herr.Message != "slow down" {
				t.Fatalf("err = %#v, want HTTPError %d with the parsed body", err, tt.wantStatus)
			}
		})
	}
}

func TestPost_WaitIsBoundedByContext(t *testing.T) {
	srv, _ := server(t, "10", 503)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Post(ctx, http.DefaultClient, srv.URL, http.Header{"X-Test": {"yes"}}, struct{}{}, &struct{}{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v, want the deadline, quickly", err, time.Since(start))
	}
}

func TestParseError_Shapes(t *testing.T) {
	for body, want := range map[string]HTTPError{
		`{"error":{"message":"bad input","type":"invalid_request_error","code":null}}`: {Type: "invalid_request_error", Message: "bad input"},
		`{"error":"model not found"}`: {Message: "model not found"},
		`not json`:                    {Message: "not json"},
	} {
		got := parseError(400, "", []byte(body))
		if got.Type != want.Type || got.Message != want.Message {
			t.Errorf("parseError(%s) = %+v, want %+v", body, got, want)
		}
	}
}

func TestParseError_KeepsCode(t *testing.T) {
	got := parseError(400, "", []byte(`{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	if got.Code != "context_length_exceeded" || !strings.Contains(got.Error(), "context_length_exceeded") {
		t.Errorf("parseError = %+v (%v), want the code kept", got, got)
	}
	if got := parseError(400, "", []byte(`{"error":{"message":"x","code":42}}`)); got.Code != "" || got.Message != "x" {
		t.Errorf("non-string code: %+v", got)
	}
}

func TestParseError_AuthMessageWithheld(t *testing.T) {
	for _, status := range []int{401, 403} {
		got := parseError(status, "", []byte(`{"error":{"message":"Incorrect API key provided: sk-ab***xyz","type":"invalid_request_error"}}`))
		if strings.Contains(got.Error(), "sk-") || got.Message != authMessage {
			t.Errorf("%d: err = %v, want the provider message withheld", status, got)
		}
	}
}

func TestTruncate_RuneBoundary(t *testing.T) {
	long := strings.Repeat("a", 511) + strings.Repeat("é", 10) // byte 512 is inside an é
	got := parseError(500, "", []byte(`{"error":"`+long+`"}`)).Message
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") || len(got) > 515 {
		t.Errorf("truncated = %q (valid UTF-8: %v)", got, utf8.ValidString(got))
	}
	if got := parseError(500, "", []byte(strings.Repeat("x", 600))).Message; len(got) != 515 {
		t.Errorf("raw body not truncated: %d bytes", len(got))
	}
}

func TestParseRetryAfter_Bounds(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"":      0,
		"2":     2 * time.Second,
		"0.5":   500 * time.Millisecond,
		"-1":    0,
		"NaN":   0,
		"Inf":   time.Hour,
		"-Inf":  0,
		"1e300": time.Hour,
		"soon":  0,
	} {
		if got := parseRetryAfter(v); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
