package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// enrollFiles writes a gateway.json and gateway.env like install.sh leaves them.
func enrollFiles(t *testing.T, api string) (cfg, env string) {
	t.Helper()
	dir := t.TempDir()
	cfg = filepath.Join(dir, "gateway.json")
	env = filepath.Join(dir, "gateway.env")
	if err := os.MkdirAll(filepath.Join(dir, "state", "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"_comment":"keep me","planId":"REPLACE_WITH_PLAN_UUID","gatewayLabel":"cannaru-pi","allowControl":false,"spoolDir":"`+filepath.Join(dir, "state", "spool")+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("# comment\nCANNABITS_API_URL="+api+"\nCANNABITS_API_KEY=cbk_replace_me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, env
}

func TestEnrollStoresTheKeyAndPlan(t *testing.T) {
	var gotCode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pro/gateways/enroll" || r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != "" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCode = body["code"]
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"gatewayId":"g-1","planId":"p-1","key":"cbk_newkey0123456789","keyPrefix":"cbk_newkey01","syncPolicy":"aggregated"}`))
	}))
	defer srv.Close()
	t.Setenv("CANNABITS_API_URL", "")
	cfg, env := enrollFiles(t, srv.URL)

	var out, errOut bytes.Buffer
	// httptest serves plain http on 127.0.0.1: the localhost flag is what lets it through
	if code := runEnroll([]string{"-config", cfg, "-env-file", env, "-insecure-localhost", "7KQ2-M9XD-4TRB"}, &out, &errOut); code != 0 {
		t.Fatalf("enroll exit %d: %s", code, errOut.String())
	}
	if gotCode != "7KQ2-M9XD-4TRB" {
		t.Errorf("code sent = %q", gotCode)
	}
	keyPath := filepath.Join(filepath.Dir(cfg), "state", "gateway.key")
	if key, _ := os.ReadFile(keyPath); string(key) != "cbk_newkey0123456789\n" {
		t.Errorf("key file: %q", key)
	}
	if rawEnv, _ := os.ReadFile(env); !strings.Contains(string(rawEnv), "CANNABITS_API_KEY=cbk_replace_me") {
		t.Errorf("the env file is left alone (the key file wins over it): %q", rawEnv)
	}
	var conf map[string]any
	rawCfg, _ := os.ReadFile(cfg)
	if err := json.Unmarshal(rawCfg, &conf); err != nil || conf["planId"] != "p-1" || conf["_comment"] != "keep me" || conf["gatewayLabel"] != "cannaru-pi" {
		t.Errorf("config: %s (%v)", rawCfg, err)
	}
	for _, p := range []string{keyPath, cfg} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", filepath.Base(p), fi.Mode().Perm())
		}
	}
	if strings.Contains(out.String(), "cbk_newkey0123456789") {
		t.Error("the key must never be printed")
	}
}

func TestEnrollRefusesPlainHTTPAndARefusedCode(t *testing.T) {
	t.Setenv("CANNABITS_API_URL", "")
	cfg, env := enrollFiles(t, "http://gateway-test.invalid")
	before, _ := os.ReadFile(env)
	var out, errOut bytes.Buffer
	if code := runEnroll([]string{"-config", cfg, "-env-file", env, "7KQ2-M9XD-4TRB"}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "https") {
		t.Fatalf("a plain http API must be refused: exit %d %s", code, errOut.String())
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"the enrollment code is unknown, used or expired"}`))
	}))
	defer srv.Close()
	cfg, env = enrollFiles(t, srv.URL)
	before, _ = os.ReadFile(env)
	errOut.Reset()
	if code := runEnroll([]string{"-config", cfg, "-env-file", env, "-insecure-localhost", "USED-CODE"}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "HTTP 401") {
		t.Fatalf("a refused code must fail: exit %d %s", code, errOut.String())
	}
	if after, _ := os.ReadFile(env); !bytes.Equal(before, after) {
		t.Error("a failed enrollment must leave the env file untouched")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "state", "gateway.key")); !os.IsNotExist(err) {
		t.Error("a failed enrollment must not write a key")
	}
}
