package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// redirectTrap is an https platform that answers every call with a 307 to a
// plain-http server, which records whether anything (and a key) reached it.
func redirectTrap(t *testing.T) (tlsURL string, transport http.RoundTripper, reached *atomic.Int32, keySeen *atomic.Value) {
	t.Helper()
	reached = &atomic.Int32{}
	keySeen = &atomic.Value{}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		keySeen.Store(r.Header.Get("X-Api-Key"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"stored":1}`))
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(secure.Close)
	return secure.URL, secure.Client().Transport, reached, keySeen
}

// The key never follows a redirect: a 307 from the platform to plain http is
// an error, and the batch stays in the spool.
func TestSendFollowsNoRedirect(t *testing.T) {
	url, transport, reached, keySeen := redirectTrap(t)
	c := NewClient(url, "p-1", "cbk_secret", "pi")
	c.HTTP.Transport = transport
	if _, err := c.Send(Batch{ID: "b1"}); err == nil {
		t.Fatal("a redirected send must fail (the batch stays spooled)")
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the redirect target was called %d time(s), key %q", n, keySeen.Load())
	}
}

// Every other platform client (agent polls, results, reports, rotation,
// enrollment) is built by NewAPIHTTPClient and refuses redirects the same way.
func TestAPIHTTPClientFollowsNoRedirect(t *testing.T) {
	url, transport, reached, _ := redirectTrap(t)
	APITransport = transport
	t.Cleanup(func() { APITransport = nil })
	req, _ := http.NewRequest(http.MethodGet, url+"/api/pro/plans/p-1/gateway/config", nil)
	req.Header.Set("X-Api-Key", "cbk_secret")
	if resp, err := NewAPIHTTPClient(0).Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("a redirect must be an error, got HTTP %d", resp.StatusCode)
	}
	if reached.Load() != 0 {
		t.Fatal("the key followed the redirect")
	}
}
