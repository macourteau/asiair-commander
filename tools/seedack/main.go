// Command seedack writes a throwaway data dir tailored to a browser/UI pass of the
// acknowledge-culled-frames feature (design library-reconciliation §10).
// It seeds one normal rig with a verified-missing group — plus a held missing row in
// the same directory, so the group button's raw count (3) differs from the dialog's
// acknowledgeable count (2) with a skipped breakdown — and one keep_originals rig
// whose controls are suppressed.
//
// NOT part of the product — a local verification aid, sibling to tools/seeddemo.
// Usage: make demo-up SEED=seedack   (login password: 12345678), open
// http://localhost:8686, and stop it with make demo-down.
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
	cfg.WebPort = 8686
	hash, err := bcrypt.GenerateFromPassword([]byte("12345678"), bcrypt.DefaultCost)
	must(err)
	cfg.PasswordHash = string(hash)
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	cfg.SessionSecret = base64.StdEncoding.EncodeToString(secret)

	cfg.Rigs = []model.RigProfile{
		{ID: "rig-fra", Name: "FRA300 Rig", Host: "127.0.0.1", User: "guest",
			OTA: "FRA300", Session: "S1", Managed: true, KeepOriginals: false,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}}},
		{ID: "rig-keep", Name: "Keep-Originals Rig", Host: "127.0.0.1", User: "guest",
			OTA: "N200", Session: "S2", Managed: true, KeepOriginals: true,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}}},
	}
	must(config.Save(filepath.Join(dir, "config.yaml"), cfg))

	st, err := store.Open(filepath.Join(dir, "state.db"))
	must(err)
	defer st.Close()
	ctx := context.Background()
	now := time.Now()

	type spec struct {
		rig, target, filt string
		state             model.FrameState
		night             int64
		missing           bool
	}
	specs := []spec{
		// rig-fra, NGC 7000 / NIGHT_3: two verified-missing (acknowledgeable) + one
		// held-missing (same dir, NOT acknowledgeable) → group button "all 3",
		// dialog count 2, skipped {not_verified:1}; the held row shows no control.
		{"rig-fra", "NGC 7000", "Ha", model.StateVerified, 3, true},
		{"rig-fra", "NGC 7000", "OIII", model.StateVerified, 3, true},
		{"rig-fra", "NGC 7000", "SII", model.StateHeld, 3, true},
		// rig-fra, M31 / NIGHT_2: verified but present (not missing) → not in panel.
		{"rig-fra", "M31", "L", model.StateVerified, 2, false},
		// rig-keep, IC 1805 / NIGHT_5: verified-missing but keep_originals → count
		// shown in the group stack, but no button, and the row shows no control.
		{"rig-keep", "IC 1805", "Ha", model.StateVerified, 5, true},
	}
	for i, s := range specs {
		mt := now.Add(-time.Duration((i+1)*7) * time.Minute)
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
			DestPath:      fmt.Sprintf("%s/%s/NIGHT_%d/Light_%s_%04d.fit", cfg.Roots.Light, s.target, s.night, s.filt, i+1),
			State:         s.state,
			PulledAt:      mt.Unix(),
		}
		must(st.UpsertFrame(ctx, f))
		if s.missing {
			must(st.SetFrameMissing(ctx, f.ID, now.Add(-5*time.Minute).Unix()))
		}
	}
	fmt.Printf("seeded %d frames into %s (web_port %d, login 12345678)\n", len(specs), dir, cfg.WebPort)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed error:", err)
		os.Exit(1)
	}
}
