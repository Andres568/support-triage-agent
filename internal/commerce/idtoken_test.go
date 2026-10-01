package commerce

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// metadataServer mints tok-1, tok-2, ... for audience and counts the mints.
func metadataServer(t *testing.T, audience string, mints *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Query().Get("audience") != audience {
			http.Error(w, "bad metadata request", http.StatusBadRequest)
			return
		}
		n := mints.Add(1)
		_, _ = w.Write([]byte("tok-" + string(rune('0'+n)) + "\n"))
	}))
	t.Cleanup(s.Close)
	return s
}

// apiServer accepts only the given token and counts every request.
func apiServer(t *testing.T, want *atomic.Value, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+want.Load().(string) {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, c *http.Client, u string) int {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestIDTokenTransport_AttachesCachedToken(t *testing.T) {
	var mints, hits atomic.Int32
	var want atomic.Value
	want.Store("tok-1")
	api := apiServer(t, &want, &hits)
	c := &http.Client{Transport: &IDTokenTransport{Audience: api.URL, MetadataURL: metadataServer(t, api.URL, &mints).URL}}
	for range 3 {
		if code := get(t, c, api.URL); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	}
	if n := mints.Load(); n != 1 {
		t.Errorf("minted %d tokens, want 1 (cached)", n)
	}
}

// A revoked token is dropped on 401 and the request retried once.
func TestIDTokenTransport_RetriesOnceAfter401(t *testing.T) {
	var mints, hits atomic.Int32
	var want atomic.Value
	want.Store("tok-1")
	api := apiServer(t, &want, &hits)
	c := &http.Client{Transport: &IDTokenTransport{Audience: api.URL, MetadataURL: metadataServer(t, api.URL, &mints).URL}}
	get(t, c, api.URL)
	want.Store("tok-2") // tok-1 revoked
	if code := get(t, c, api.URL); code != http.StatusOK {
		t.Fatalf("status %d after refresh, want 200", code)
	}
	want.Store("never")
	if code := get(t, c, api.URL); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want the 401 after one retry", code)
	}
	if n := hits.Load(); n != 5 { // 1 + (1 + retry) + (1 + retry)
		t.Errorf("api hits = %d, want 5", n)
	}
}

// No token, no request: the API is never called, and the error names the
// cause without echoing the metadata server's body.
func TestIDTokenTransport_MetadataErrorFailsRequest(t *testing.T) {
	var hits atomic.Int32
	var want atomic.Value
	want.Store("tok-1")
	api := apiServer(t, &want, &hits)
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal-metadata-detail", http.StatusNotFound)
	}))
	defer metadata.Close()

	c := &http.Client{Transport: &IDTokenTransport{Audience: api.URL, MetadataURL: metadata.URL}}
	_, err := c.Get(api.URL)
	if err == nil || !strings.Contains(err.Error(), "commerce ID token") {
		t.Fatalf("err = %v, want a commerce ID token error", err)
	}
	if strings.Contains(err.Error(), "internal-metadata-detail") {
		t.Errorf("error leaks the metadata body: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("api hits = %d, want 0", n)
	}
}

// The token only goes to the audience's host.
func TestIDTokenTransport_OtherHostGetsNoToken(t *testing.T) {
	var mints atomic.Int32
	var gotAuth atomic.Value
	gotAuth.Store("")
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()
	aud := "https://commerce.example"
	c := &http.Client{Transport: &IDTokenTransport{Audience: aud, MetadataURL: metadataServer(t, aud, &mints).URL}}
	get(t, c, other.URL)
	if a := gotAuth.Load().(string); a != "" || mints.Load() != 0 {
		t.Errorf("other host got Authorization %q after %d mints, want none", a, mints.Load())
	}
}
