// Command seedremote stages the remote bridge's every card state for the UI:
// remotes Waiting, Connected (direct and relayed), Reconnecting, Unreachable and
// Needs update on each side, a stale remote build, a remote with warnings, the
// rigs reached through them in every rig-card state, a device found through a
// remote, and Commander's own relay reachability down and up, beside a local rig.
//
// It writes a data dir (config.yaml plus a remote scenario, seedremote.json) for
// the `-tags demo` build of Commander, which plays each paired remote as an
// in-memory remote process instead of a tailcat tunnel
// (cmd/asiair-commander/demo_remote.go). Only the tunnel client is replaced, so
// the supervisor, the rig workers and the hub derive every state as they do in
// production. Live states are reached at runtime, through the timeline markers
// tools/seedremote/uipass.js drops, not written into config or the store.
//
// NOT part of the product — a local verification aid, sibling to tools/seedscan.
//
// Usage: go run ./tools/seedremote <data-dir>
//
//	(login password: 12345678, port 8790)
//
// The data dir must be empty or a previous seedremote dir: a re-run starts
// afresh, dropping the previous run's store and markers.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/demoscenario"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// webPort is this seeder's port in the CLAUDE.md table.
const webPort = 8790

// remoteSpec is one remote to pair, and what its process does.
type remoteSpec struct {
	name string
	seed demoscenario.Remote
	rigs []rigSpec
}

// rigSpec is one rig reached through a remote. The addresses are a
// documentation-style private subnet and are never dialled: the demo device
// factory serves the rig's files from memory.
type rigSpec struct {
	id, name, guid, ip string
	readDelayMS        int
	preload            int
}

// quickReads is the read delay of the rigs whose remote drops during the
// pass. At the demo's own pace a frame copies slower than the next one lands,
// so a cycle's backlog, and the session it holds open, grows without bound;
// these rigs must end their cycle soon after their remote drops to show the
// remote-down card.
const quickReads = 40

func intp(v int) *int { return &v }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seedremote <data-dir>")
		os.Exit(1)
	}
	dir := os.Args[1]
	must(prepareDir(dir))

	now := time.Now().UTC()
	fresh := proto.Build{Version: "v1.25.0", Commit: "demo", Date: now.Add(-48 * time.Hour).Format(time.RFC3339)}

	specs := []remoteSpec{
		// Never connected: the user has not installed its .env yet.
		{name: "Spring Camp", seed: demoscenario.Remote{Running: false}},
		// Connected direct, with warnings: a refused forward at its rig's
		// address and a rejected client key (a leaked pairing), plus an ASIAIR
		// on its LAN nobody has adopted.
		{name: "Hilltop Observatory", seed: demoscenario.Remote{
			Running: true, Direct: true, RelayRegion: "nyc", LatencyMS: 38, Build: fresh, UptimeS: 3 * 86400,
			Warnings: []proto.Warning{
				{Kind: proto.WarnRefusedForward, Proto: "tcp", Target: "192.168.20.30:445", AgeMS: 40 * 60 * 1000,
					Reason: "no broadcast round has run since the remote started"},
				{Kind: proto.WarnRejectedClient, ClientKey: "nodekey:5e1f0c2a9d8b7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f", AgeMS: 2 * 3600 * 1000,
					Reason: "a client key other than the pinned one tried to connect; the pairing may have leaked"},
			},
			Devices: []demoscenario.Device{{GUID: "hl0unadopt1", CPUID: "beef0000hl0000a1", IP: "192.168.20.41", SSID: "MyRig-Hilltop"}},
		}, rigs: []rigSpec{{id: "rig-hill", name: "Hilltop RedCat", guid: "hl0redcat01", ip: "192.168.20.30"}}},
		// Connected relayed.
		{name: "Dark Sky Lodge", seed: demoscenario.Remote{
			Running: true, Direct: false, RelayRegion: "nyc", LatencyMS: 165, Build: fresh, UptimeS: 11 * 3600,
		}, rigs: []rigSpec{{id: "rig-lodge", name: "Lodge Esprit", guid: "lg0esprit01", ip: "192.168.30.20"}}},
		// Needs update (remote): an older data-unit digest, and a build more
		// than two weeks behind Commander's.
		{name: "Pier Shed", seed: demoscenario.Remote{
			Running: true, Direct: true, RelayRegion: "nyc", LatencyMS: 52, UptimeS: 40 * 86400,
			Build:             proto.Build{Version: "v1.12.0", Commit: "demo", Date: now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)},
			DataDigestVersion: intp(header.DataDigestVersion - 1),
		}},
		// Needs update (Commander): a newer whole-file digest.
		{name: "Farm Barn", seed: demoscenario.Remote{
			Running: true, Direct: true, RelayRegion: "nyc", LatencyMS: 61, UptimeS: 5 * 3600,
			Build:              proto.Build{Version: "v1.26.0", Commit: "demo", Date: now.Format(time.RFC3339)},
			WholeDigestVersion: intp(header.WholeDigestVersion + 1),
		}},
		// Stopped late in the pass, so it reads Reconnecting with its rig's
		// last-known line.
		{name: "Valley Field", seed: demoscenario.Remote{
			Running: true, Direct: true, RelayRegion: "nyc", LatencyMS: 44, Build: fresh, UptimeS: 7 * 86400,
		}, rigs: []rigSpec{{id: "rig-valley", name: "Valley Askar", guid: "vf0askar001", ip: "192.168.40.21", readDelayMS: quickReads}}},
		// Stopped early, so it is Unreachable by capture time: one rig shows its
		// last-known line, the other was paused before the drop.
		{name: "Ridge Pier", seed: demoscenario.Remote{
			Running: true, Direct: false, RelayRegion: "nyc", LatencyMS: 120, Build: fresh, UptimeS: 2 * 86400,
		}, rigs: []rigSpec{
			{id: "rig-ridge", name: "Ridge FSQ", guid: "rp0fsq00001", ip: "192.168.50.31", readDelayMS: quickReads},
			{id: "rig-newt", name: "Ridge Newtonian", guid: "rp0newt0001", ip: "192.168.50.32", readDelayMS: quickReads},
		}},
		// Stopped with Valley Field, and its rig holds a live session through
		// the drop: its reads are slow and its device full, so its cycle
		// outlasts the pass. It has a remote of its own because a remote site
		// admits one transfer at a time, so a sibling rig would queue behind
		// this one's and never end its own cycle.
		{name: "Creek Shed", seed: demoscenario.Remote{
			Running: true, Direct: true, RelayRegion: "nyc", LatencyMS: 48, Build: fresh, UptimeS: 6 * 86400,
		}, rigs: []rigSpec{{id: "rig-seestar", name: "Creek Seestar", guid: "ck0seestar1", ip: "192.168.45.22", readDelayMS: 4000, preload: 8}}},
		// Re-paired during the pass and never given its new .env, so it reads
		// Waiting for first connection with a rig that has a last-known line.
		{name: "Desert Camp", seed: demoscenario.Remote{
			Running: true, Direct: true, RelayRegion: "nyc", LatencyMS: 70, Build: fresh, UptimeS: 9 * 3600,
		}, rigs: []rigSpec{{id: "rig-desert", name: "Desert Askar", guid: "dc0askar001", ip: "192.168.60.40", readDelayMS: quickReads}}},
	}

	cfg := baseConfig(dir)
	sc := &demoscenario.Scenario{}
	for i, sp := range specs {
		id := fmt.Sprintf("r%d", i+1)
		p, err := tunnel.Mint(tunnel.FakeRegion())
		must(err)
		cfg.Remotes = append(cfg.Remotes, model.RemoteProfile{
			ID: id, Name: sp.name,
			Address: p.Address.Reveal(), ClientKey: p.ClientKey.Reveal(),
			// The address names the fake relay region, which is never dialled;
			// the card shows this display region.
			Region: "nyc", RegionName: "New York City",
			CreatedAt: now.Add(-time.Duration(30-i) * 24 * time.Hour).Unix(),
		})
		seed := sp.seed
		seed.ID, seed.EnvValue, seed.Address = id, p.EnvValue.Reveal(), p.Address.Reveal()
		for _, rg := range sp.rigs {
			seed.Devices = append(seed.Devices, demoscenario.Device{GUID: rg.guid, CPUID: "beef" + rg.guid, IP: rg.ip, SSID: rg.name})
			sc.Rigs = append(sc.Rigs, demoscenario.Rig{GUID: rg.guid, Site: id, IP: rg.ip, ReadDelayMS: rg.readDelayMS, Preload: rg.preload})
			cfg.Rigs = append(cfg.Rigs, rigProfile(rg.id, rg.name, rg.guid))
		}
		sc.Remotes = append(sc.Remotes, seed)
	}
	cfg.RemoteSeq = len(specs)

	must(config.Save(filepath.Join(dir, "config.yaml"), cfg))
	must(demoscenario.Save(dir, sc))

	fmt.Printf(`seedremote ready
  data-dir  %s
  build     go build -tags demo -ldflags "-X main.version=$(git describe --tags --always) -X main.buildTime=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" -o <bin> ./cmd/asiair-commander
  run       <bin> -data-dir %s        # http://localhost:%d, password 12345678
  pass      OUT=<out-dir> AC_DATA_DIR=%s NODE_PATH=$(npm root -g) node tools/seedremote/uipass.js
`, dir, dir, webPort, dir)
}

// baseConfig is the config every seeded rig and remote is added to: a fast
// poll so the rigs list within seconds, and a short unreachable threshold so
// the pass reaches Unreachable in minutes.
func baseConfig(dir string) *model.Config {
	cfg := config.Defaults()
	cfg.WebPort = webPort
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
	cfg.Defaults.PollInterval = model.Duration(4 * time.Second)
	cfg.Defaults.MinFileAge = 0
	cfg.Defaults.SettleCycles = 1
	cfg.Defaults.UnreachableAlertAfter = model.Duration(2 * time.Minute)
	// The live-session rig holds one transfer slot for the whole pass.
	cfg.MaxConcurrentTransfers = 8
	cfg.Rigs = []model.RigProfile{rigProfile("rig-home", "Home FRA300", "demo-home")}
	return cfg
}

// rigProfile is a managed rig on the demo device with this GUID. It keeps its
// originals, so no delete measures a device copy through a remote. The host
// is never contacted; config validation wants one.
func rigProfile(id, name, guid string) model.RigProfile {
	return model.RigProfile{
		ID: id, Name: name, GUID: guid, Host: "asiair.invalid", User: "guest",
		OTA: "OTA", Session: "S1", Managed: true, KeepOriginals: true,
		Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
	}
}

// prepareDir makes dir ready for a fresh run. It refuses a non-empty dir that
// is not a previous seedremote dir, so a mistyped path can never wipe a real
// data dir's store.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(dir, demoscenario.File)); err != nil {
			return fmt.Errorf("%s is not empty and holds no %s; refusing to seed over it", dir, demoscenario.File)
		}
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", demoscenario.ControlDir} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return os.MkdirAll(filepath.Join(dir, demoscenario.ControlDir), 0o700)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
