// Command seeddemo writes a throwaway data dir (config + state.db) with a login
// password and a spread of persisted frames, so the SPA can be screenshotted with
// realistic content. NOT part of the product — a local screenshot aid.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/demofiles"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

func main() {
	dir := os.Getenv("SEED_DIR")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "SEED_DIR required")
		os.Exit(1)
	}
	must(os.MkdirAll(dir, 0o700))

	cfg := config.Defaults()
	for _, sub := range []string{"light", "cal", "staging", "logs"} {
		must(os.MkdirAll(filepath.Join(dir, sub), 0o755))
	}
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("12345678"), bcrypt.DefaultCost)
	must(err)
	cfg.PasswordHash = string(hash)
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	cfg.SessionSecret = base64.StdEncoding.EncodeToString(secret)

	// SEED_LIVE=1 tailors the config for the `-tags demo` runtime: rigs carry a GUID
	// (so the demo resolver/generator drive them) and delete-after-grace is ON with
	// fast timings, so the feed shows the whole pending→transferring→filed→deleted
	// arc in seconds. Without it, two loopback rigs sit offline — fine for shooting
	// the static, seeded surfaces with a normal (non-demo) build.
	live := os.Getenv("SEED_LIVE") == "1"
	if live {
		cfg.Defaults.PollInterval = model.Duration(4 * time.Second)
		cfg.Defaults.MinFileAge = 0
		cfg.Defaults.SettleCycles = 1
		cfg.Defaults.Grace = model.Duration(15 * time.Second)
	}
	cfg.Rigs = []model.RigProfile{
		{ID: "rig-fra", Name: "FRA300 Rig", GUID: guidIf(live, demofiles.RigFRA), Host: "127.0.0.1", User: "guest",
			OTA: "FRA300", Session: "S1", Managed: true, KeepOriginals: !live,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}}},
		{ID: "rig-newt", Name: "Newtonian Rig", GUID: guidIf(live, demofiles.RigNewt), Host: "127.0.0.1", User: "guest",
			OTA: "N200", Session: "S2", Managed: true, KeepOriginals: !live,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}}},
	}
	must(config.Save(filepath.Join(dir, "config.yaml"), cfg))

	st, err := store.Open(filepath.Join(dir, "state.db"))
	must(err)
	defer st.Close()
	ctx := context.Background()
	now := time.Now()

	// In live mode the demo generator produces every frame end-to-end, so seeding
	// standalone DB rows (whose source/NAS files don't exist) would only trip
	// "frame lost" on the first delete pass. The one exception is the held frame
	// the demo build stages under the watched Autorun for the device file browser.
	if live {
		for _, rigID := range []string{"rig-fra", "rig-newt"} {
			must(st.UpsertFrame(ctx, heldFrame(dir, rigID)))
		}
		fmt.Printf("live demo config written to %s (one held frame per rig)\n", dir)
		return
	}

	type spec struct {
		rig, target, filt string
		state             model.FrameState
		minsAgo           int
		night             int64
	}
	specs := []spec{
		{"rig-fra", "NGC 7000", "Ha", model.StateDeleted, 4, 3},
		{"rig-fra", "NGC 7000", "OIII", model.StateVerified, 9, 3},
		{"rig-fra", "NGC 7000", "SII", model.StateVerified, 14, 3},
		{"rig-fra", "M31", "L", model.StateDeleted, 40, 2},
		{"rig-fra", "M31", "R", model.StateUnclassified, 55, 2},
		{"rig-newt", "IC 1805", "Ha", model.StateVerified, 6, 5},
		{"rig-newt", "IC 1805", "OIII", model.StateDeleted, 21, 5},
		{"rig-newt", "M42", "L", model.StateHeld, 70, 1},
		{"rig-newt", "M42", "B", model.StateFailed, 2, 0}, // pulled_at 0 → device panel only
	}
	for i, s := range specs {
		mt := now.Add(-time.Duration(s.minsAgo) * time.Minute)
		f := &model.Frame{
			RigID:       s.rig,
			SourceShare: "EMMC Images",
			SourcePath:  fmt.Sprintf("Autorun/Light/%s/Light_%s_%04d.fit", s.target, s.filt, i+1),
			SHA256:      fmt.Sprintf("%064x", i*7919+1),
			Size:        41_500_000 + int64(i)*130_000,
			// The archive side describes the NAS copy; a seeded row's two copies are
			// the same bytes, so both anchors carry the same values.
			ArchiveSHA256: fmt.Sprintf("%064x", i*7919+1),
			ArchiveSize:   41_500_000 + int64(i)*130_000,
			Mtime:         mt.Unix(),
			FrameType:     "Light",
			Target:        s.target,
			Filter:        s.filt,
			Exptime:       300,
			Gain:          100,
			Night:         s.night,
			DateObs:       mt.UTC().Format(time.RFC3339),
			DestPath:      fmt.Sprintf("%s/light/%s/NIGHT_%d/Light_%s_%04d.fit", dir, s.target, s.night, s.filt, i+1),
			State:         s.state,
		}
		if s.state != model.StateFailed {
			f.PulledAt = mt.Unix()
		}
		if s.state == model.StateDeleted {
			f.DeletedAt = mt.Add(30 * time.Second).Unix()
		}
		if s.state == model.StateFailed {
			f.Error = "stream copy failed; will retry"
			f.Attempts = 2
		}
		must(st.UpsertFrame(ctx, f))
	}

	// One calibration frame: filed under the cal root with no target at all. That is
	// the row shape the history and retarget views must survive — the wire omits an
	// empty target, so a reader that assumes the field is present blows up on it.
	flatMt := now.Add(-90 * time.Minute)
	must(st.UpsertFrame(ctx, &model.Frame{
		RigID:         "rig-fra",
		SourceShare:   "EMMC Images",
		SourcePath:    "Autorun/Flat/Flat_Ha_0001.fit",
		SHA256:        fmt.Sprintf("%064x", 99991),
		Size:          41_500_000,
		ArchiveSHA256: fmt.Sprintf("%064x", 99991),
		ArchiveSize:   41_500_000,
		Mtime:         flatMt.Unix(),
		FrameType:     "Flat",
		Filter:        "Ha",
		Exptime:       2,
		Gain:          100,
		Night:         2,
		DateObs:       flatMt.UTC().Format(time.RFC3339),
		DestPath:      fmt.Sprintf("%s/cal/flat/Flat_Ha_0001.fit", dir),
		State:         model.StateVerified,
		PulledAt:      flatMt.Unix(),
	}))
	fmt.Printf("seeded %d frames into %s\n", len(specs)+1, dir)
}

// heldFrame is rigID's row for demofiles.HeldFrame, in the held state. decide()
// skips a held row whose size and mtime match the listing and the delete pass
// never selects one, so the device file stays and the device file browser's
// delete dialog counts it as unarchived on every run, however long the live
// demo has been draining frames.
func heldFrame(dir, rigID string) *model.Frame {
	hf := demofiles.HeldFrame
	sum := fmt.Sprintf("%064x", 424242)
	return &model.Frame{
		RigID:         rigID,
		SourceShare:   demofiles.Share,
		SourcePath:    hf.Path,
		SHA256:        sum,
		Size:          int64(hf.Size),
		ArchiveSHA256: sum,
		ArchiveSize:   int64(hf.Size),
		Mtime:         hf.Mtime.Unix(),
		FrameType:     "Light",
		Target:        "M42",
		Filter:        "L",
		Exptime:       300,
		Gain:          100,
		DateObs:       hf.Mtime.UTC().Format(time.RFC3339),
		DestPath:      filepath.Join(dir, "light", "M42", demofiles.FullName),
		State:         model.StateHeld,
		PulledAt:      hf.Mtime.Unix(),
		Error:         "delete-pass three-way check failed (device/NAS/recorded mismatch)",
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed error:", err)
		os.Exit(1)
	}
}

// guidIf returns g in live mode, else "" — a GUID marks a rig the demo runtime
// drives; without it the rig is a static, offline placeholder.
func guidIf(live bool, g string) string {
	if live {
		return g
	}
	return ""
}
