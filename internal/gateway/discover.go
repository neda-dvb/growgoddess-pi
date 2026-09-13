package gateway

// Discovery: find OptiClimate boxes on the gateway's own LAN, and remember
// them by hardware address. Facilities hand out addresses by DHCP, so a box
// can come back on another IP after a power cut; the MAC is the identity
// that survives that. Read-only: a scan is a TCP connect per host on the
// controller port, then the same GETs the source uses.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// DiscoveredController is one box that answered on the controller port.
type DiscoveredController struct {
	Host    string             `json:"host"`
	Port    int                `json:"port"`
	MAC     string             `json:"mac,omitempty"`
	Name    string             `json:"name,omitempty"`
	Units   []DiscoveredUnit   `json:"units"`
	AirTemp *float64           `json:"airTemp,omitempty"`
	RH      *float64           `json:"rh,omitempty"`
	Program string             `json:"program,omitempty"`
	Values  map[string]float64 `json:"-"`
}

// DiscoveredUnit is one air unit behind a box (getOptiClimateList).
type DiscoveredUnit struct {
	Name    string `json:"name"`
	Address int    `json:"address"`
}

// DiscoveryResult is what a scan found and how far it looked.
type DiscoveryResult struct {
	Controllers  []DiscoveredController `json:"controllers"`
	ScannedHosts int                    `json:"scannedHosts"`
	DurationMs   int64                  `json:"durationMs"`
	Subnets      []string               `json:"subnets"`
}

// LocalIPv4Hosts lists every host address of the /24 networks this machine
// sits on (non-loopback, up interfaces). A wider mask is clamped to the /24
// around the interface address, so a scan never exceeds 254 hosts per
// interface.
func LocalIPv4Hosts() (hosts []string, subnets []string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			base := fmt.Sprintf("%d.%d.%d.", ip4[0], ip4[1], ip4[2])
			if seen[base] {
				continue
			}
			seen[base] = true
			subnets = append(subnets, base+"0/24")
			for i := 1; i <= 254; i++ {
				h := fmt.Sprintf("%s%d", base, i)
				if h != ip4.String() {
					hosts = append(hosts, h)
				}
			}
		}
	}
	return hosts, subnets
}

// DiscoverOptiClimate probes every host on the controller port, in
// parallel, and identifies the boxes that answer the OptiClimate API. It
// never writes. dialTimeout bounds each connect; parallel bounds concurrency.
func DiscoverOptiClimate(ctx context.Context, hosts []string, port int, dialTimeout time.Duration, parallel int) DiscoveryResult {
	started := time.Now()
	if parallel <= 0 {
		parallel = 64
	}
	if dialTimeout <= 0 {
		dialTimeout = 400 * time.Millisecond
	}
	sem := make(chan struct{}, parallel)
	var mu sync.Mutex
	var found []DiscoveredController
	var wg sync.WaitGroup
	// fresh connection per box: a pooled half-dead connection from an
	// earlier poll must never make a live box look absent
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}}
	for _, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(host string) {
			defer wg.Done()
			defer func() { <-sem }()
			d := net.Dialer{Timeout: dialTimeout}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
			if err != nil {
				return
			}
			conn.Close()
			c, ok := identifyOptiClimate(client, host, port)
			if !ok {
				return
			}
			mu.Lock()
			found = append(found, c)
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	// MAC addresses come from the neighbour table the dials just populated
	arp := readARPTable()
	for i := range found {
		found[i].MAC = arp[found[i].Host]
	}
	sort.Slice(found, func(i, j int) bool { return ipLess(found[i].Host, found[j].Host) })
	return DiscoveryResult{Controllers: found, ScannedHosts: len(hosts), DurationMs: time.Since(started).Milliseconds()}
}

// identifyOptiClimate asks a host that accepts connections on the port
// whether it speaks the OptiClimate API, and what it reads right now.
func identifyOptiClimate(client *http.Client, host string, port int) (DiscoveredController, bool) {
	base := fmt.Sprintf("http://%s:%d", host, port)
	resp, err := client.Get(base + "/backend/getOptiClimateList")
	if err != nil {
		return DiscoveredController{}, false
	}
	defer resp.Body.Close()
	var list struct {
		GetOptiClimateList struct {
			OptiClimates []DiscoveredUnit `json:"optiClimates"`
		} `json:"getOptiClimateList"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&list) != nil {
		return DiscoveredController{}, false
	}
	out := DiscoveredController{Host: host, Port: port, Units: list.GetOptiClimateList.OptiClimates, Values: map[string]float64{}}
	if out.Units == nil {
		out.Units = []DiscoveredUnit{}
	}
	values, err := optiClimateGetValues(client, base, 0, []string{"OptiClimateName", "Room1Temp", "Humidity", "ContrStatus"})
	if err == nil {
		out.Name = stringValue(values["OptiClimateName"])
		if f, ok := numericValue(values["Room1Temp"]); ok {
			v := round1(f)
			out.AirTemp = &v
		}
		if f, ok := numericValue(values["Humidity"]); ok {
			v := round1(f)
			out.RH = &v
		}
		out.Program = stringValue(values["ContrStatus"])
	}
	return out, true
}

// MACFor returns the hardware address the neighbour table holds for an IP
// this machine talked to recently, lower-case, or "" when unknown.
func MACFor(host string) string { return readARPTable()[host] }

// readARPTable reads the kernel neighbour table on Linux (/proc/net/arp).
// Other systems return an empty table; discovery still works, without MACs.
func readARPTable() map[string]string {
	out := map[string]string{}
	if runtime.GOOS != "linux" {
		return out
	}
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return out
	}
	defer f.Close()
	return parseARP(f)
}

// parseARP decodes /proc/net/arp: "IP address HW type Flags HW address Mask
// Device"; flags 0x0 means an incomplete entry with no usable address.
func parseARP(r interface{ Read([]byte) (int, error) }) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[2] == "0x0" {
			continue
		}
		mac := strings.ToLower(fields[3])
		if mac == "00:00:00:00:00:00" {
			continue
		}
		out[fields[0]] = mac
	}
	return out
}

// ipLess orders dotted IPv4 addresses numerically.
func ipLess(a, b string) bool {
	ia, ib := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if ia == nil || ib == nil {
		return a < b
	}
	for i := 0; i < 4; i++ {
		if ia[i] != ib[i] {
			return ia[i] < ib[i]
		}
	}
	return false
}
