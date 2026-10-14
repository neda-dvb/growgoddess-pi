package gateway

// Edge security: what the gateway refuses on its own, whatever the backend
// or a config file asks.
//
//   - The platform API is reached over https only (plain http only for a
//     localhost API, and only with an explicit flag).
//   - A setpoint outside the gateway's own guard band is never written.
//   - A controller address is reached only when it lies in the gateway's
//     controller networks (private ranges by default) or was found by this
//     gateway's own LAN scan.

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"strings"
)

// ValidateAPIURL accepts an https URL with a host. A plain http URL passes
// only when allowInsecureLocalhost is set and the host is localhost or a
// loopback address (a development backend on the same machine).
func ValidateAPIURL(raw string, allowInsecureLocalhost bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("api URL %q is not a URL with a host", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		loopback := host == "localhost"
		if a, err := netip.ParseAddr(host); err == nil && a.IsLoopback() {
			loopback = true
		}
		if loopback && allowInsecureLocalhost {
			return nil
		}
		if loopback {
			return fmt.Errorf("api URL %q is plain http; for a local development backend start with -insecure-localhost", raw)
		}
		return fmt.Errorf("api URL %q is plain http; the gateway sends data over https only", raw)
	}
	return fmt.Errorf("api URL %q: scheme must be https", raw)
}

// GuardBand is the gateway's own limit on setpoint writes, checked before
// the controller is touched. Zero fields take the defaults: 15 to 32 °C and
// 30 to 85 %RH (the platform's pro.control.limits band).
type GuardBand struct {
	TempMin float64 `json:"tempMin,omitempty"`
	TempMax float64 `json:"tempMax,omitempty"`
	RHMin   float64 `json:"rhMin,omitempty"`
	RHMax   float64 `json:"rhMax,omitempty"`
}

// DefaultGuardBand is the band a gateway uses when gateway.json sets none.
var DefaultGuardBand = GuardBand{TempMin: 15, TempMax: 32, RHMin: 30, RHMax: 85}

// ErrOutsideGuardBand: the value is outside the gateway's own band; nothing
// was read or written on the controller.
var ErrOutsideGuardBand = errors.New("outside the gateway's guard band")

// Effective fills unset fields from the defaults.
func (b GuardBand) Effective() GuardBand {
	if b.TempMin == 0 {
		b.TempMin = DefaultGuardBand.TempMin
	}
	if b.TempMax == 0 {
		b.TempMax = DefaultGuardBand.TempMax
	}
	if b.RHMin == 0 {
		b.RHMin = DefaultGuardBand.RHMin
	}
	if b.RHMax == 0 {
		b.RHMax = DefaultGuardBand.RHMax
	}
	return b
}

// Problems reports a band that cannot be meant: min not below max, or
// outside what the quantities can be.
func (b GuardBand) Problems() []string {
	e := b.Effective()
	var out []string
	if e.TempMin >= e.TempMax || e.TempMin < 0 || e.TempMax > 50 {
		out = append(out, fmt.Sprintf("guardBand: temperature %g to %g °C must be rising and inside 0 to 50", e.TempMin, e.TempMax))
	}
	if e.RHMin >= e.RHMax || e.RHMin < 0 || e.RHMax > 100 {
		out = append(out, fmt.Sprintf("guardBand: humidity %g to %g %%RH must be rising and inside 0 to 100", e.RHMin, e.RHMax))
	}
	return out
}

// Check refuses a setpoint value outside the band. Temperature setpoints
// start with temp_, humidity ones with rh_; any other key is refused.
func (b GuardBand) Check(key string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%w: %v is not a number", ErrOutsideGuardBand, value)
	}
	e := b.Effective()
	lo, hi, unit := 0.0, 0.0, ""
	switch {
	case strings.HasPrefix(key, "temp_"):
		lo, hi, unit = e.TempMin, e.TempMax, "°C"
	case strings.HasPrefix(key, "rh_"):
		lo, hi, unit = e.RHMin, e.RHMax, "%RH"
	default:
		return fmt.Errorf("%w: no band for %q", ErrOutsideGuardBand, key)
	}
	if value < lo || value > hi {
		return fmt.Errorf("%w (%g to %g %s)", ErrOutsideGuardBand, lo, hi, unit)
	}
	return nil
}

// DefaultControllerNetworks are the private and link-local ranges: RFC 1918,
// IPv4 link-local, IPv6 unique local and link-local.
var DefaultControllerNetworks = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "fc00::/7", "fe80::/10"}

// ParseControllerNetworks turns gateway.json's controllerNetworks into
// prefixes. Empty means the defaults. Every network must lie inside a
// private or link-local range: an allowlist cannot open the public internet.
func ParseControllerNetworks(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) == 0 {
		cidrs = DefaultControllerNetworks
	}
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return nil, fmt.Errorf("controllerNetworks: %q is not a network like 192.168.2.0/24", c)
		}
		p = p.Masked()
		a := p.Addr()
		if !(a.IsPrivate() || a.IsLinkLocalUnicast()) || p.Bits() < privateBits(a) {
			return nil, fmt.Errorf("controllerNetworks: %s is not inside a private or link-local range", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// privateBits is the shortest prefix that still stays inside the private
// range containing a (so 10.0.0.0/7 is refused while 10.0.0.0/8 passes).
func privateBits(a netip.Addr) int {
	switch {
	case a.Is4() && a.As4()[0] == 10:
		return 8
	case a.Is4() && a.As4()[0] == 172:
		return 12
	case a.Is4() && a.As4()[0] == 192:
		return 16
	case a.Is4(): // 169.254/16
		return 16
	case a.IsLinkLocalUnicast(): // fe80::/10
		return 10
	default: // fc00::/7
		return 7
	}
}

// HostInNetworks reports whether host is an IP inside one of nets. Host
// names are never in a network: the gateway does not resolve names the
// backend sends it.
func HostInNetworks(host string, nets []netip.Prefix) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	for _, n := range nets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

// Sync policies: what this gateway sends to the cloud.
const (
	SyncFull       = "full"
	SyncAggregated = "aggregated"
	SyncLocalOnly  = "local_only"
)

// ValidSyncPolicy reports whether p is a known policy ("" counts as full).
func ValidSyncPolicy(p string) bool {
	return p == "" || p == SyncFull || p == SyncAggregated || p == SyncLocalOnly
}

// StricterPolicy returns the policy that sends less: local_only over
// aggregated over full. An unknown value counts as local_only, so a typo
// never sends more than intended.
func StricterPolicy(a, b string) string {
	rank := func(p string) int {
		switch p {
		case "", SyncFull:
			return 0
		case SyncAggregated:
			return 1
		}
		return 2
	}
	if rank(a) >= rank(b) {
		if rank(a) == 2 {
			return SyncLocalOnly
		}
		return orFull(a)
	}
	if rank(b) == 2 {
		return SyncLocalOnly
	}
	return orFull(b)
}

func orFull(p string) string {
	if p == "" {
		return SyncFull
	}
	return p
}
