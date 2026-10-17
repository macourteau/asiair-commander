package discovery

import (
	"encoding/json"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// The scan_air exchange is the UDP broadcast discovery protocol the ZWO ASIAIR
// app uses. A small JSON request is broadcast to 255.255.255.255:scanPort and
// each device answers with a JSON-RPC object whose result carries the stable
// device guid plus model, current ip, and a hardware cpuId. Confirmed against
// real hardware (ASIAIR Plus, RK3568): the reply is JSON-RPC-wrapped (fields
// under result, not top-level) and there is no advertised name field. Each site
// runs the broadcast round itself (netsite.Site.Broadcast); discovery parses
// what comes back.
const (
	// scanPort is the UDP port scan_air requests and replies use.
	scanPort = netsite.ScanAirPort
)

// scanRequest is the unicast scan_air request, the same bytes the local site
// broadcasts.
var scanRequest = netsite.ScanAirRequest

// scannedDevice is one device's reply, distilled to the identity fields the
// registry keys on. There is no advertised display name, but SSID — the device's
// own Wi-Fi name — carries identity a label derived from model plus a short guid
// cannot: several ASIAIRs of the same generation report a byte-identical model
// string, while the SSID is the one field their owner sets and recognises.
type scannedDevice struct {
	GUID    string
	CPUID   string
	Model   string
	Address string
	SSID    string
	// Site is the site the reply arrived through; the zero value is the local
	// network.
	Site netsite.ID
	// PathLatency is the site's minimum recent latency when the reply arrived,
	// the figure the path rule compares between two sites seeing one device.
	PathLatency time.Duration
	// Raw is the datagram as it arrived, kept so an operator-triggered scan can
	// log what a device actually said. Fields this struct drops (is_pi4,
	// connect_lock) are often what identifies an unexpected answer.
	Raw string
}

// endpoint is where the device answered: its address on the site the reply
// came through.
func (d scannedDevice) endpoint() netsite.Endpoint {
	return netsite.Endpoint{Site: d.Site, IP: d.Address}
}

// scanReply is the JSON-RPC envelope of a scan_air reply. Unknown fields
// (is_pi4, connect_lock, jsonrpc, id) are ignored.
type scanReply struct {
	Method string `json:"method"`
	Code   int    `json:"code"`
	Result struct {
		GUID  string `json:"guid"`
		CPUID string `json:"cpuId"`
		Model string `json:"model"`
		IP    string `json:"ip"`
		SSID  string `json:"ssid"`
	} `json:"result"`
}

// parseReply reads one scan_air datagram. srcAddr is the datagram's UDP source
// host, used as the address when the reply omits result.ip. A reply is dropped
// (ok=false) when it does not parse, its method is not scan_air, its code is
// non-zero, or its result.guid is empty.
func parseReply(data []byte, srcAddr string) (scannedDevice, bool) {
	var r scanReply
	if err := json.Unmarshal(data, &r); err != nil {
		return scannedDevice{}, false
	}
	if r.Method != "scan_air" || r.Code != 0 || r.Result.GUID == "" {
		return scannedDevice{}, false
	}
	addr := r.Result.IP
	if addr == "" {
		addr = srcAddr
	}
	return scannedDevice{
		GUID:    r.Result.GUID,
		CPUID:   r.Result.CPUID,
		Model:   r.Result.Model,
		Address: addr,
		SSID:    r.Result.SSID,
		Raw:     string(data),
	}, true
}

// ScanAirIdentity is what a remote reads from a scan_air reply: the device's
// identity and the address it answered for. Address is the reply's result.ip,
// or the datagram's source when the reply omits it.
type ScanAirIdentity struct {
	GUID    string
	CPUID   string
	Address string
}

// ParseScanAirReply reads one scan_air datagram by exactly the rules discovery
// applies (parseReply), so a remote admits to its forwarding allowlist only a
// reply Commander would accept as a device.
func ParseScanAirReply(data []byte, srcAddr string) (ScanAirIdentity, bool) {
	d, ok := parseReply(data, srcAddr)
	if !ok {
		return ScanAirIdentity{}, false
	}
	return ScanAirIdentity{GUID: d.GUID, CPUID: d.CPUID, Address: d.Address}, true
}
