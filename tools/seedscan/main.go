// Command seedscan reproduces the network-scan device count against a LAN with
// no real hardware. It seeds a data-dir with two managed rigs and then answers
// scan_air broadcasts as two fake ASIAIRs — one bound to a rig (managed), one
// unadopted — and silences the managed one on cue so it goes stale.
//
// That timeline is the whole point. The discovery registry keeps an unmanaged
// device for ten minutes after it stops answering and keeps a managed one
// indefinitely, so a rig that is powered off mid-session stays in the list a
// scan returns. Counting that list reports devices that are switched off; only
// the entries inside the staleness window (135s) actually answered. The Go side
// is correct — the registry is supposed to retain — so the reproduction is a UI
// one, which is why this drives a browser rather than asserting in a test.
//
// A real ASIAIR on the same LAN answers these broadcasts too and simply adds to
// the count; uipass.js asserts on the change across rounds, not on an absolute
// number, so it stays valid either way.
//
// NOT part of the product — a local verification aid, sibling to tools/seednight.
//
// Usage: go run ./tools/seedscan <data-dir>
//
//	(login password: 12345678, port 8789; runs in the foreground until ^C)
//
// The managed device stops answering when <data-dir>/quiet appears, so the
// browser pass controls the timeline rather than racing a timer: `touch` it to
// drive this by hand, or let uipass.js create it between its two scans.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
)

// scanPort is the UDP port the scan_air exchange uses, mirroring
// internal/discovery. The responder binds it on every interface, so the host's
// own broadcast reaches it.
const scanPort = 4720

// fake is one simulated ASIAIR. The address is deliberately a bogus subnet: it
// is only ever displayed, never contacted, so a typo cannot reach real hardware.
type fake struct {
	guid    string
	cpuID   string
	address string
	// ssid is the device's own Wi-Fi name. These fakes share one model string, as
	// real same-generation hardware does, so the ssid is the only thing telling
	// them apart in the UI — which is the point of seeding distinct ones.
	ssid string
}

var (
	// managedDev is bound to a rig in the seeded config, so it is the one that
	// survives in the registry after it stops answering — the case the scan count must not include.
	managedDev = fake{guid: "f4a00001", cpuID: "beef00000000f4a0", address: "10.77.0.11", ssid: "FRA300-Pier"}
	// spareDev is never adopted, so it holds the "Found on your network" list and
	// keeps answering throughout, giving the second round a nonzero count.
	spareDev = fake{guid: "5pa00002", cpuID: "beef000000005pa0", address: "10.77.0.12", ssid: "MyRig"}
	// namelessDev answers with no ssid, the case where naming has to fall back to
	// the model-plus-guid label. It is never adopted, so the found list shows both
	// spellings of a device name side by side.
	namelessDev = fake{guid: "0no00003", cpuID: "beef0000000000n0", address: "10.77.0.13"}
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seedscan <data-dir>")
		os.Exit(1)
	}
	dir := os.Args[1]
	must(os.MkdirAll(dir, 0o700))

	seedConfig(dir)

	// A leftover marker from a previous run would silence the managed device
	// before the first scan, so the reproduction would start already finished.
	quietMarker := filepath.Join(dir, "quiet")
	must(ignoreMissing(os.Remove(quietMarker)))

	// active is the set of devices still answering; the managed one is dropped
	// when the quiet marker appears.
	var mu sync.Mutex
	active := []fake{managedDev, spareDev, namelessDev}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: scanPort})
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen on :%d failed (is another scan_air responder running?): %v\n", scanPort, err)
		os.Exit(1)
	}
	defer conn.Close()

	go func() {
		for {
			if _, err := os.Stat(quietMarker); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		mu.Lock()
		active = []fake{spareDev, namelessDev}
		mu.Unlock()
		fmt.Printf("%s  device %s went quiet — it stays in the registry, so the count must "+
			"drop within the 135s staleness window\n", time.Now().Format("15:04:05"), managedDev.guid)
	}()

	local := hostAddresses()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // socket closed on shutdown
			}
			var req struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(buf[:n], &req); err != nil || req.Method != "scan_air" {
				continue
			}
			if !isLocalRequest(src.IP, local) {
				fmt.Printf("ignoring scan from %s — another Commander on this LAN would "+
					"otherwise discover these fakes as real devices\n", src.IP)
				continue
			}
			mu.Lock()
			answering := append([]fake(nil), active...)
			mu.Unlock()
			for _, d := range answering {
				if _, err := conn.WriteToUDP(reply(d), src); err != nil {
					fmt.Fprintf(os.Stderr, "reply to %s failed: %v\n", src, err)
				}
			}
		}
	}()

	fmt.Printf(`seedscan ready
  data-dir  %s
  password  12345678   port 8789
  answering as %s/%s (bound to rig-fra; goes quiet on %s), %s/%s and %s (unadopted,
  the last with no ssid at all) — one model string between them, so the ssid is
  the only thing telling them apart

Run the app against this data-dir from another terminal, then drive the browser:
  make demo-up TAGS= DEMO_DATA=%s
  OUT=<out-dir> AC_DATA_DIR=%s AC_PASSWORD=12345678 TARGET=http://localhost:8789 \
    NODE_PATH=$(npm root -g) node tools/seedscan/uipass.js
  make demo-down DEMO_DATA=%s
^C to stop.
`, dir, managedDev.guid, managedDev.ssid, quietMarker,
		spareDev.guid, spareDev.ssid, namelessDev.guid, dir, dir, dir)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\nseedscan stopping")
}

// hostAddresses returns the IP addresses this machine holds. The responder
// answers requests from these only: the broadcast it is meant to hear comes from
// the Commander running beside it, and every other scan on the LAN belongs to a
// real instance that must not discover these fakes as hardware.
func hostAddresses() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot enumerate local addresses (%v) — answering nothing, "+
			"which is the safe direction\n", err)
		return out
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			out[ipnet.IP.String()] = true
		}
	}
	return out
}

// isLocalRequest reports whether a scan came from this machine. Loopback counts
// even when it is not in the interface list.
func isLocalRequest(src net.IP, local map[string]bool) bool {
	return src.IsLoopback() || local[src.String()]
}

// reply renders one device's scan_air answer, matching the JSON-RPC envelope real
// hardware sends (fields under result, code 0).
func reply(d fake) []byte {
	return fmt.Appendf(nil,
		`{"jsonrpc":"2.0","method":"scan_air","result":{"guid":%q,"cpuId":%q,"ip":%q,`+
			`"model":"ZWO AirPlus-RK3568 (Linux)","is_pi4":true,"ssid":%q,"connect_lock":false},"code":0,"id":1}`,
		d.guid, d.cpuID, d.address, d.ssid)
}

// seedConfig writes a data-dir holding two managed rigs: one bound to the fake
// device that goes quiet, and one bound to a guid nothing ever answers — the
// rig that is configured but has never been seen, which is why the count and the
// rig list disagree in the first place.
func seedConfig(dir string) {
	cfg := config.Defaults()
	cfg.WebPort = 8789
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	for _, p := range []string{cfg.Roots.Light, cfg.Roots.Cal, cfg.Roots.Staging, cfg.Roots.Logs} {
		must(os.MkdirAll(p, 0o755))
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("12345678"), bcrypt.MinCost)
	must(err)
	cfg.PasswordHash = string(hash)
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	cfg.SessionSecret = base64.StdEncoding.EncodeToString(secret)
	// The hosts are never contacted: the fake devices answer discovery only, and a
	// managed rig must name a host or config validation refuses the file.
	cfg.Rigs = []model.RigProfile{
		{
			ID: "rig-fra", Name: "FRA300 Rig", OTA: "FRA300", Session: "S1",
			GUID: managedDev.guid, CPUID: managedDev.cpuID,
			Host: "asiair-fra.invalid", User: "guest",
			Managed: true, KeepOriginals: true,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
		},
		{
			ID: "rig-uc", Name: "UC91 Rig", OTA: "UC91", Session: "S1",
			GUID: "unseen001", CPUID: "beef00000000un5e",
			Host: "asiair-uc.invalid", User: "guest",
			Managed: true, KeepOriginals: true,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
		},
	}
	must(config.Save(filepath.Join(dir, "config.yaml"), cfg))
}

// ignoreMissing drops a not-exists error, so removing an absent marker is not
// itself a failure.
func ignoreMissing(err error) error {
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
