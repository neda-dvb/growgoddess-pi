package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The gateway rotates with overlap: it asks with its current key, stores the
// new one, then confirms with the new one.
func TestRotateKeyStoresThenConfirms(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path+" "+r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/api/pro/plans/p-1/gateway/rotate":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "cbk_new", "keyPrefix": "cbk_new"})
		case "/api/pro/plans/p-1/gateway/rotate/confirm":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	keyPath := filepath.Join(t.TempDir(), "gateway.key")

	next, err := rotateKey(srv.URL, "p-1", "cbk_old", keyPath)
	if err != nil || next != "cbk_new" {
		t.Fatalf("rotate: %q %v", next, err)
	}
	if len(calls) != 2 || calls[0] != "/api/pro/plans/p-1/gateway/rotate cbk_old" || calls[1] != "/api/pro/plans/p-1/gateway/rotate/confirm cbk_new" {
		t.Fatalf("ask with the old key, confirm with the new one: %v", calls)
	}
	if raw, _ := os.ReadFile(keyPath); string(raw) != "cbk_new\n" {
		t.Fatalf("stored key = %q", raw)
	}

	// the new key cannot be stored: keep the current one, never confirm
	calls = nil
	next, err = rotateKey(srv.URL, "p-1", "cbk_old", filepath.Join(t.TempDir(), "missing", "gateway.key"))
	if err == nil || next != "" {
		t.Fatalf("an unstorable key must fail the rotation: %q %v", next, err)
	}
	for _, c := range calls {
		if c == "/api/pro/plans/p-1/gateway/rotate/confirm cbk_new" {
			t.Fatal("a key that was not stored must never be confirmed")
		}
	}
}
