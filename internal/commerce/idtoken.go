package commerce

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// metadataIdentityURL is where Cloud Run mints ID tokens for the service's
// own service account.
const metadataIdentityURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

// idTokenRefresh is under the token's one-hour lifetime, so a cached token
// never expires mid-request.
const idTokenRefresh = 45 * time.Minute

// IDTokenTransport attaches `Authorization: Bearer <ID token>` for Audience
// (the commerce-api URL), which Cloud Run's IAM check requires because the
// service has no allUsers invoker. Only used when COMMERCE_API_AUDIENCE is set.
type IDTokenTransport struct {
	Audience    string
	Base        http.RoundTripper // nil: http.DefaultTransport
	MetadataURL string            // "": the metadata server

	mu      sync.Mutex
	token   string
	fetched time.Time
}

// RoundTrip attaches the token only to requests for the audience's host, so
// a redirect or a misconfigured URL never carries it elsewhere. A 401 means
// the cached token was revoked or expired early: it is dropped and the
// request retried once with a fresh one.
func (t *IDTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if aud, err := url.Parse(t.Audience); err != nil || req.URL.Host != aud.Host {
		return base.RoundTrip(req)
	}
	resp, err := t.send(base, req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || (req.Body != nil && req.GetBody == nil) {
		return resp, err
	}
	_ = resp.Body.Close()
	t.clear()
	retry := req
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		retry = req.Clone(req.Context())
		retry.Body = body
	}
	return t.send(base, retry)
}

func (t *IDTokenTransport) send(base http.RoundTripper, req *http.Request) (*http.Response, error) {
	tok, err := t.idToken(req.Context())
	if err != nil {
		if req.Body != nil { // a RoundTripper must close the body, even on error
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("commerce ID token: %w", err)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return base.RoundTrip(r)
}

func (t *IDTokenTransport) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
}

// idToken holds the mutex across the metadata fetch on purpose: concurrent
// requests wait for one mint instead of each minting their own. The fetch is
// bounded by the request's context, and the metadata server is local.

func (t *IDTokenTransport) idToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Since(t.fetched) < idTokenRefresh {
		return t.token, nil
	}
	endpoint := t.MetadataURL
	if endpoint == "" {
		endpoint = metadataIdentityURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?audience="+url.QueryEscape(t.Audience), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server: %s", resp.Status)
	}
	t.token, t.fetched = strings.TrimSpace(string(body)), time.Now()
	return t.token, nil
}
