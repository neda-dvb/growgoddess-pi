package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestValidateAPIURLRefusesPlainHTTP(t *testing.T) {
	cases := []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://api.gateway-test.invalid", false, true},
		{"https://api.gateway-test.invalid/", false, true},
		{"http://api.gateway-test.invalid", false, false},
		{"http://api.gateway-test.invalid", true, false}, // the flag is for localhost only
		{"http://192.168.2.10:8080", true, false},
		{"http://localhost:8080", false, false},
		{"http://localhost:8080", true, true},
		{"http://127.0.0.1:8080", true, true},
		{"http://[::1]:8080", true, true},
		{"ftp://api.gateway-test.invalid", true, false},
		{"api.gateway-test.invalid", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		err := ValidateAPIURL(c.url, c.insecure)
		if (err == nil) != c.ok {
			t.Errorf("ValidateAPIURL(%q, insecure=%v) = %v, want ok=%v", c.url, c.insecure, err, c.ok)
		}
	}
}

func TestGuardBandRefusesOutsideEvenIfAsked(t *testing.T) {
	var zero GuardBand // unset in gateway.json: the defaults, never "no band"
	for _, c := range []struct {
		key   string
		value float64
		ok    bool
	}{
		{"temp_day", 15, true}, {"temp_day", 32, true}, {"temp_night", 14.9, false}, {"temp_day", 33, false},
		{"rh_day", 30, true}, {"rh_night", 85, true}, {"rh_day", 29, false}, {"rh_night", 100, false},
		{"co2_setpoint", 800, false},
	} {
		err := zero.Check(c.key, c.value)
		if (err == nil) != c.ok {
			t.Errorf("default band %s=%g: %v, want ok=%v", c.key, c.value, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrOutsideGuardBand) {
			t.Errorf("refusal must be ErrOutsideGuardBand, got %v", err)
		}
	}
	narrow := GuardBand{TempMin: 20, TempMax: 28}
	if narrow.Check("temp_day", 29) == nil || narrow.Check("temp_day", 25) != nil || narrow.Check("rh_day", 60) != nil {
		t.Error("a configured band applies; unset fields keep the defaults")
	}
	if len((GuardBand{TempMin: 30, TempMax: 20}).Problems()) == 0 || len((GuardBand{RHMax: 120}).Problems()) == 0 {
		t.Error("an inverted or impossible band is a config problem")
	}
	if len(zero.Problems()) != 0 {
		t.Error("the default band is valid")
	}
}

// The band is checked before the controller is touched at all.
func TestSetSetpointOutsideGuardBandNeverReachesController(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL}
	if _, err := ctl.SetSetpoint("temp_day", 40); !errors.Is(err, ErrOutsideGuardBand) {
		t.Fatalf("40 °C must be refused by the guard band, got %v", err)
	}
	ctl.Band = GuardBand{RHMin: 40, RHMax: 70}
	if _, err := ctl.SetSetpoint("rh_day", 75); !errors.Is(err, ErrOutsideGuardBand) {
		t.Fatalf("75 %%RH must be refused by a 40-70 band, got %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the controller was called %d times for refused values", n)
	}
}

func TestControllerNetworks(t *testing.T) {
	def, err := ParseControllerNetworks(nil)
	if err != nil || len(def) != len(DefaultControllerNetworks) {
		t.Fatalf("defaults: %v %v", def, err)
	}
	for host, want := range map[string]bool{
		"192.168.2.110": true, "10.1.2.3": true, "172.16.5.4": true, "169.254.1.1": true,
		"fe80::1": true, "fd00::5": true, "::ffff:192.168.2.110": true,
		"172.32.0.1": false, "198.51.100.8": false, "127.0.0.1": false, "::1": false, "0.0.0.0": false,
		"opticlimate.local": false, "evil.invalid": false, "": false,
	} {
		if got := HostInNetworks(host, def); got != want {
			t.Errorf("default networks: %q allowed=%v, want %v", host, got, want)
		}
	}
	narrow, err := ParseControllerNetworks([]string{"192.168.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if HostInNetworks("192.168.3.4", narrow) || !HostInNetworks("192.168.2.200", narrow) {
		t.Error("a narrowed list allows exactly its network")
	}
	for _, bad := range [][]string{{"0.0.0.0/0"}, {"198.51.100.0/24"}, {"10.0.0.0/7"}, {"192.168.0.0/15"}, {"fc00::/6"}, {"127.0.0.0/8"}, {"not-a-cidr"}} {
		if _, err := ParseControllerNetworks(bad); err == nil {
			t.Errorf("controllerNetworks %v must be refused", bad)
		}
	}
}

func TestStricterPolicy(t *testing.T) {
	for _, c := range [][3]string{
		{"", "", SyncFull}, {SyncFull, "", SyncFull}, {"", SyncAggregated, SyncAggregated},
		{SyncAggregated, SyncFull, SyncAggregated}, {SyncFull, SyncLocalOnly, SyncLocalOnly},
		{SyncLocalOnly, SyncAggregated, SyncLocalOnly}, {"everything", SyncFull, SyncLocalOnly},
	} {
		if got := StricterPolicy(c[0], c[1]); got != c[2] {
			t.Errorf("StricterPolicy(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestEdgeProblems(t *testing.T) {
	ok := Config{API: "https://api.gateway-test.invalid"}
	if p := ok.EdgeProblems(false); len(p) != 0 {
		t.Fatalf("a plain https config has no problems: %v", p)
	}
	bad := Config{API: "http://api.gateway-test.invalid", SyncPolicy: "everything", ControllerNetworks: []string{"0.0.0.0/0"}, GuardBand: GuardBand{TempMin: 40}}
	if p := bad.EdgeProblems(true); len(p) != 4 {
		t.Fatalf("want 4 problems (http, band, networks, policy), got %d: %v", len(p), p)
	}
}
