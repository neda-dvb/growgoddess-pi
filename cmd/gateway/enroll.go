// Enrollment: a new gateway redeems the one-time code the owner created in
// Grow Goddess Pro and receives its own key. Run once, as root, on the Pi:
//
//	sudo cannabits-gateway enroll 7KQ2-M9XD-4TRB
//
// The key goes into gateway.key next to the spool (/var/lib/cannabits-gateway,
// mode 0600, owned like that directory, so the service can rotate it later);
// it wins over CANNABITS_API_KEY. The plan id goes into gateway.json, which
// keeps its owner. The API URL comes from CANNABITS_API_URL in the
// environment, then in the env file, then "api" in gateway.json, and must be
// https. Re-running enroll with a new code (a replaced Pi) replaces the key;
// the old one stops working at that moment.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neda-dvb/growgoddess-pi/internal/gateway"
)

type enrollResult struct {
	GatewayID  string `json:"gatewayId"`
	PlanID     string `json:"planId"`
	Key        string `json:"key"`
	KeyPrefix  string `json:"keyPrefix"`
	SyncPolicy string `json:"syncPolicy"`
}

func runEnroll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/etc/cannabits/gateway.json", "gateway configuration file (receives planId)")
	envPath := fs.String("env-file", "/etc/cannabits/gateway.env", "environment file (read for CANNABITS_API_URL)")
	insecureLocalhost := fs.Bool("insecure-localhost", false, "allow a plain http API URL on localhost (development only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: cannabits-gateway enroll [-config f] [-env-file f] <code>")
		return 2
	}
	code := fs.Arg(0)

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "enroll: read config: %v\n", err)
		return 1
	}
	var conf map[string]any
	if err := json.Unmarshal(raw, &conf); err != nil {
		fmt.Fprintf(stderr, "enroll: parse config: %v\n", err)
		return 1
	}
	api := strings.TrimSpace(os.Getenv("CANNABITS_API_URL"))
	if api == "" {
		api = envFileValue(*envPath, "CANNABITS_API_URL")
	}
	if api == "" {
		api, _ = conf["api"].(string)
	}
	if err := gateway.ValidateAPIURL(api, *insecureLocalhost); err != nil {
		fmt.Fprintf(stderr, "enroll: %v\n", err)
		return 1
	}
	spoolDir, _ := conf["spoolDir"].(string)
	if spoolDir == "" {
		fmt.Fprintln(stderr, "enroll: gateway.json has no spoolDir; the key is kept next to the spool")
		return 1
	}
	keyPath := gateway.Config{SpoolDir: spoolDir}.StateKeyPath()
	stateDir := filepath.Dir(keyPath)
	if fi, err := os.Stat(stateDir); err != nil || !fi.IsDir() {
		fmt.Fprintf(stderr, "enroll: %s does not exist; run install.sh first\n", stateDir)
		return 1
	}

	res, err := redeemEnrollment(api, code)
	if err != nil {
		fmt.Fprintf(stderr, "enroll: %v\n", err)
		return 1
	}
	if err := storeKey(keyPath, stateDir, res.Key); err != nil {
		// the code is spent: say plainly what to do, never print the key
		fmt.Fprintf(stderr, "enroll: the platform issued a key but it could not be stored: %v\nCreate a new enrollment code in Grow Goddess Pro and run enroll again.\n", err)
		return 1
	}
	conf["planId"] = res.PlanID
	out, _ := json.MarshalIndent(conf, "", "  ")
	if err := writeKeepingOwner(*configPath, append(out, '\n')); err != nil {
		fmt.Fprintf(stderr, "enroll: key stored, but writing planId %s to %s failed: %v\n", res.PlanID, *configPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "enrolled: gateway %s for plan %s, key %s... stored in %s, sync policy %s\nnext: sudo systemctl restart cannabits-gateway\n",
		res.GatewayID, res.PlanID, res.KeyPrefix, keyPath, res.SyncPolicy)
	return 0
}

func redeemEnrollment(api, code string) (enrollResult, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	url := strings.TrimRight(api, "/") + "/api/pro/gateways/enroll"
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return enrollResult{}, err
	}
	defer resp.Body.Close()
	var out enrollResult
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return out, fmt.Errorf("the platform refused the code (HTTP %d): %s", resp.StatusCode, e.Error)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	if !strings.HasPrefix(out.Key, "cbk_") || out.PlanID == "" {
		return out, fmt.Errorf("the platform's answer has no key or plan")
	}
	return out, nil
}

// envFileValue reads NAME=value from a systemd environment file ("" when
// absent).
func envFileValue(path, name string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, name+"="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// storeKey writes the key file (0600, temp then rename) and gives it the
// owner of the state directory, so the service user can read and rotate it.
func storeKey(keyPath, stateDir, key string) error {
	if err := gateway.WriteKeyFile(keyPath, key); err != nil {
		return err
	}
	return keepOwner(stateDir, keyPath)
}

// writeKeepingOwner replaces a file atomically with mode 0600 and, when it
// existed, its previous owner (so the service user can still read
// gateway.json after root ran enroll).
func writeKeepingOwner(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := keepOwner(path, tmp.Name()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
