// Package demoscenario is the contract between tools/seedremote, which writes a data
// dir's remote scenario, and the `-tags demo` build of Commander, which plays it
// (cmd/asiair-commander/demo_remote.go). It is NOT part of the product: only the
// seeder and the demo build import it.
package demoscenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/macourteau/asiair-commander/internal/remote/proto"
)

// File is the scenario's name in the data dir. A demo build whose data dir has
// none dials every remote through its real tunnel client.
const File = "seedremote.json"

// ControlDir, under the data dir, holds the markers that drive the timeline.
// The demo build polls it, so a browser pass (or a `touch` by hand) decides
// when each step happens rather than racing a timer.
const ControlDir = "seedremote-ctl"

// LinkDown is the marker standing for Commander's own internet being down: no
// tunnel client reaches its relay, and every tunnel flow fails, until it is
// removed.
const LinkDown = "link-down"

// StopMarker is the marker that stops remote id's process for good, as
// `docker compose down` on the remote PC would.
func StopMarker(id string) string { return "stop-" + id }

// Scenario is every remote the demo build plays, and the rigs reached through
// them.
type Scenario struct {
	Remotes []Remote `json:"remotes"`
	Rigs    []Rig    `json:"rigs"`
}

// Remote is one remote PC and the process it runs.
type Remote struct {
	// ID is the remote's id in config.yaml.
	ID string `json:"id"`
	// EnvValue is the pairing the remote's .env holds, and Address the
	// address that pairing was minted with: a profile naming any other
	// address is a re-pair the remote never installed.
	EnvValue string `json:"env_value"`
	Address  string `json:"address"`
	// Running is whether the remote process is up when Commander starts; a
	// remote that is not running reads Waiting for first connection.
	Running bool `json:"running"`
	// Direct and RelayRegion are the path the remote reports in hello, and
	// LatencyMS the round trip each hello takes.
	Direct      bool   `json:"direct"`
	RelayRegion string `json:"relay_region"`
	LatencyMS   int    `json:"latency_ms"`
	// Build is the remote's binary, as hello reports it.
	Build proto.Build `json:"build"`
	// WholeDigestVersion and DataDigestVersion are the digest algorithms the
	// remote reports; nil reports Commander's own.
	WholeDigestVersion *int `json:"whole_digest_version,omitempty"`
	DataDigestVersion  *int `json:"data_digest_version,omitempty"`
	// UptimeS is the process's uptime when Commander starts.
	UptimeS int64 `json:"uptime_s"`
	// Warnings is the remote's recent warnings; each AgeMS is its age when
	// Commander starts.
	Warnings []proto.Warning `json:"warnings,omitempty"`
	// Devices answer the remote's scan_air broadcast on its LAN.
	Devices []Device `json:"devices,omitempty"`
}

// Device is one ASIAIR on a remote's LAN.
type Device struct {
	GUID  string `json:"guid"`
	CPUID string `json:"cpu_id"`
	IP    string `json:"ip"`
	SSID  string `json:"ssid"`
}

// Rig is a configured rig reached through a remote: its device is the demo
// build's in-memory fake keyed by GUID, found at IP on Site.
type Rig struct {
	GUID string `json:"guid"`
	Site string `json:"site"`
	IP   string `json:"ip"`
	// ReadDelayMS overrides the demo throttle's delay per read chunk, so one
	// rig's session can be kept open across the whole pass.
	ReadDelayMS int `json:"read_delay_ms,omitempty"`
	// Preload is how many frames sit on the device before the first cycle.
	Preload int `json:"preload,omitempty"`
}

// Model is the model string every seeded device reports.
const Model = "ZWO AirPlus-RK3568 (Linux)"

// ScanAirReply is d's answer to a scan_air broadcast, in the JSON-RPC shape a
// real ASIAIR sends (internal/discovery parses it).
func ScanAirReply(d Device) []byte {
	return fmt.Appendf(nil,
		`{"jsonrpc":"2.0","method":"scan_air","result":{"guid":%q,"cpuId":%q,"ip":%q,`+
			`"model":%q,"is_pi4":true,"ssid":%q,"connect_lock":false},"code":0,"id":1}`,
		d.GUID, d.CPUID, d.IP, Model, d.SSID)
}

// Save writes s into dataDir.
func Save(dataDir string, s *Scenario) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, File), b, 0o600)
}

// Load reads dataDir's scenario; nil with no error when there is none.
func Load(dataDir string) (*Scenario, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, File))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	return &s, nil
}
