package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDiscoverOptiClimate: a box answering the OptiClimate API is found with
// its units and live values; a host with the port closed is not; a host
// answering HTTP but not the API is not.
func TestDiscoverOptiClimate(t *testing.T) {
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/backend/getOptiClimateList"):
			_, _ = w.Write([]byte(`{"getOptiClimateList":{"optiClimates":[{"name":"OptiClimate 1","address":0}]}}`))
		case strings.HasSuffix(r.URL.Path, "/backend/getRegisterValues"):
			_, _ = w.Write([]byte(`{"getRegisterValues":{"address":0,"values":{"OptiClimateName":{"value":"OptiClimate 1"},"Room1Temp":{"value":28.9},"Humidity":{"value":63.0},"ContrStatus":{"value":"Day"}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer box.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(box.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	// a second server on another port: open, HTTP, but not an OptiClimate
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()

	res := DiscoverOptiClimate(context.Background(), []string{"127.0.0.1", "127.0.0.2"}, port, 300*time.Millisecond, 8)
	if res.ScannedHosts != 2 {
		t.Fatalf("scanned = %d, want 2", res.ScannedHosts)
	}
	if len(res.Controllers) != 1 {
		t.Fatalf("found %d controllers, want exactly the box: %+v", len(res.Controllers), res.Controllers)
	}
	c := res.Controllers[0]
	if c.Host != "127.0.0.1" || c.Port != port || c.Name != "OptiClimate 1" || c.Program != "Day" {
		t.Errorf("identity = %+v", c)
	}
	if len(c.Units) != 1 || c.Units[0].Address != 0 {
		t.Errorf("units = %+v", c.Units)
	}
	if c.AirTemp == nil || *c.AirTemp != 28.9 || c.RH == nil || *c.RH != 63 {
		t.Errorf("live values = %v %v", c.AirTemp, c.RH)
	}
	// the non-OptiClimate HTTP server must not be reported
	_, otherPort, _ := net.SplitHostPort(strings.TrimPrefix(other.URL, "http://"))
	op, _ := strconv.Atoi(otherPort)
	res2 := DiscoverOptiClimate(context.Background(), []string{"127.0.0.1"}, op, 300*time.Millisecond, 8)
	if len(res2.Controllers) != 0 {
		t.Errorf("a plain HTTP server must not be reported as a controller: %+v", res2.Controllers)
	}
}

func TestParseARP(t *testing.T) {
	sample := `IP address       HW type     Flags       HW address            Mask     Device
192.168.2.110    0x1         0x2         24:18:C6:20:89:91     *        wlan0
192.168.2.113    0x1         0x0         00:00:00:00:00:00     *        wlan0
192.168.2.1      0x1         0x2         64:dd:68:ae:6d:c0     *        wlan0
`
	got := parseARP(strings.NewReader(sample))
	if got["192.168.2.110"] != "24:18:c6:20:89:91" {
		t.Errorf("lower-cased MAC expected, got %q", got["192.168.2.110"])
	}
	if _, ok := got["192.168.2.113"]; ok {
		t.Error("an incomplete entry (flags 0x0) must be skipped")
	}
	if got["192.168.2.1"] != "64:dd:68:ae:6d:c0" {
		t.Errorf("router entry = %q", got["192.168.2.1"])
	}
}

func TestIPLess(t *testing.T) {
	if !ipLess("192.168.2.9", "192.168.2.110") {
		t.Error("numeric order, not lexical")
	}
	hosts, _ := LocalIPv4Hosts()
	for _, h := range hosts {
		if net.ParseIP(h) == nil {
			t.Fatalf("bad host %q", h)
		}
	}
}
