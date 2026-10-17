// Command seednight seeds a data-dir whose archive exercises every arm of the
// retarget preview's night projection (design
// routing-and-filing §6.5), so the preview card can be driven and photographed
// in a real browser.
//
// The retarget destination is the target "NGC7380", already filed across three
// night folders, and three mis-labelled "NGC 7830" frames are staged to land in
// each arm when retargeted to it:
//
//	NIGHT_1  the ledger already assigned this observing date  → exact, no caveat
//	NIGHT_3  only the destination FOLDER holds the date       → adopted, caveat
//	NIGHT_4  a date nothing has shot                          → minted,  caveat
//
// NIGHT_3 holds the same target spelled "NGC 7380" — space-stripping files both
// spellings into one folder while the OBJECT cards stay distinct — so the row
// that lands there also reports the divergent spelling already filed. A fourth
// frame already carrying "NGC7380" is there to be selected as an ineligible row,
// which puts the problem-styled refusal beside the informational sub-lines.
//
// NOT part of the product — a local verification aid, sibling to tools/seedack.
// The archive paths are written into the preview verbatim, so seed into a SHORT
// directory or the mobile overflow reading is about the path, not the layout.
//
// Usage: go run ./tools/seednight <data-dir>   (login password: 12345678, port 8788)
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
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	ota     = "FRA300"
	session = "S1"
	rigID   = "rig-01"
	// destTarget is what the operator types into Retarget; altSpelling is the same
	// object as another OBJECT card, which strip-spaces files into the same folder.
	destTarget  = "NGC7380"
	altSpelling = "NGC 7380"
	// wrongTarget is the Autorun name the mis-labelled frames were captured under.
	wrongTarget = "NGC 7830"
)

// spec is one seeded frame: a raw OBJECT card, the night folder it is filed in,
// and the local calendar date of its observing night.
type spec struct {
	target string
	night  int
	day    string // observing date, YYYY-MM-DD, local
	filter string
}

// specs are seeded in order, and the frame list is read newest-id first, so the
// three retarget candidates sit at the top of the Tools table.
var specs = []spec{
	// The destination archive: NGC7380 as itself on two nights…
	{destTarget, 1, "2026-07-10", "Ha"},
	{destTarget, 1, "2026-07-10", "OIII"},
	{destTarget, 2, "2026-07-13", "Ha"},
	{destTarget, 2, "2026-07-13", "OIII"},
	// …and a third night filed under the other spelling of the same object.
	{altSpelling, 3, "2026-07-16", "Ha"},
	{altSpelling, 3, "2026-07-16", "SII"},
	// The mis-labelled frames to retarget, one per projection arm.
	{wrongTarget, 1, "2026-07-10", "Ha"}, // → NIGHT_1, ledger-exact
	{wrongTarget, 2, "2026-07-16", "Ha"}, // → NIGHT_3, adopted from the folder
	{wrongTarget, 3, "2026-07-22", "Ha"}, // → NIGHT_4, minted
}

// ledger is night_counters as the filing path would have left it. The two
// spellings are separate scopes — which is exactly why the folder, not the
// ledger, has to answer for the NIGHT_3 case.
var ledger = []store.NightEntry{
	{Target: destTarget, OTA: ota, ObservingDate: "2026-07-10", Night: 1},
	{Target: destTarget, OTA: ota, ObservingDate: "2026-07-13", Night: 2},
	{Target: altSpelling, OTA: ota, ObservingDate: "2026-07-16", Night: 3},
	{Target: wrongTarget, OTA: ota, ObservingDate: "2026-07-10", Night: 1},
	{Target: wrongTarget, OTA: ota, ObservingDate: "2026-07-16", Night: 2},
	{Target: wrongTarget, OTA: ota, ObservingDate: "2026-07-22", Night: 3},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seednight <data-dir>")
		os.Exit(1)
	}
	dir := os.Args[1]
	must(os.MkdirAll(dir, 0o700))

	cfg := config.Defaults()
	cfg.WebPort = 8788
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
	// The host is never contacted — every seeded frame is already archived — but a
	// managed rig must name one, or config validation refuses the file.
	cfg.Rigs = []model.RigProfile{{
		ID: rigID, Name: "FRA300 Rig", OTA: ota, Session: session,
		Host: "asiair-night.invalid", User: "guest",
		Managed: true, KeepOriginals: true,
		Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
	}}
	must(config.Save(filepath.Join(dir, "config.yaml"), cfg))

	st, err := store.Open(filepath.Join(dir, "state.db"))
	must(err)
	defer st.Close()
	ctx := context.Background()
	must(st.RebuildNightCounters(ctx, ledger))

	profile := cfg.Rigs[0]
	now := time.Now().Unix()
	for i, s := range specs {
		obsDay, perr := time.ParseInLocation("2006-01-02", s.day, time.Local)
		must(perr)
		// 22:00 local: minus the 12h observing-date offset it lands mid-morning of
		// the same calendar day, so this capture belongs to the night named by s.day.
		captured := obsDay.Add(22 * time.Hour)
		obs := route.ObservingDate(captured, time.Local)
		if key := route.ObservingDateKey(obs); key != s.day {
			// A DST edge would put the capture on the wrong night and silently break
			// the arm this frame stages.
			fmt.Fprintf(os.Stderr, "fatal: %s at 22:00 resolves to observing date %s\n", s.day, key)
			os.Exit(1)
		}

		cls := model.Classification{
			Type: model.TypeLight, Target: s.target, TargetPresent: true,
			Filter: s.filter, FilterPresent: true,
			Exptime: 300, ExptimePresent: true,
			Gain: 100, GainPresent: true,
			DateObs: captured, DateObsPresent: true,
		}
		res, rerr := route.Route(cls, profile, route.Options{
			Templates:         cfg.Templates,
			Night:             s.night,
			ObservingDate:     obs,
			DateFormat:        cfg.DateFormat,
			StripTargetSpaces: cfg.StripTargetSpaces,
			MosaicDetection:   cfg.MosaicDetection,
		})
		must(rerr)

		name := fmt.Sprintf("Light_%s_300.0s_Bin1_%s_gain100_%s_-10.0C_%04d.fit",
			s.target, s.filter, captured.UTC().Format("20060102-150405"), i+1)
		destDir := filepath.Join(cfg.Roots.Light, filepath.FromSlash(res.Dir))
		must(os.MkdirAll(destDir, 0o755))
		dest := filepath.Join(destDir, name)
		// A placeholder byte, not a real FITS: nothing in the preview path opens the
		// file, and an unreadable header is what keeps the startup night rebuild from
		// deriving anything from these stand-ins.
		must(os.WriteFile(dest, []byte("seednight placeholder\n"), 0o644))
		info, serr := os.Stat(dest)
		must(serr)

		f := &model.Frame{
			RigID:       rigID,
			SourceShare: "EMMC Images",
			SourcePath:  "Autorun/" + s.target + "/" + name,
			SHA256:      fmt.Sprintf("%064x", i*7919+1),
			Size:        info.Size(),
			// The archive side describes the NAS copy; a seeded row's two copies are
			// the same bytes, so both anchors carry the same values.
			ArchiveSHA256: fmt.Sprintf("%064x", i*7919+1),
			ArchiveSize:   info.Size(),
			Mtime:         now - int64((len(specs)-i)*600),
			FrameType:     string(model.TypeLight),
			Target:        s.target,
			Filter:        s.filter,
			Exptime:       300,
			Gain:          100,
			SetTemp:       -10,
			Binning:       "Bin1",
			Sensor:        "IMX571",
			Night:         int64(s.night),
			DateObs:       captured.UTC().Format(time.RFC3339Nano),
			DestPath:      dest,
			State:         model.StateVerified,
			PulledAt:      now - int64((len(specs)-i)*600),
		}
		must(st.UpsertFrame(ctx, f))
		fmt.Printf("  %-10s %s  -> %s\n", s.target, s.day, res.Dir)
	}

	fmt.Printf("\nseeded %d frames into %s\n", len(specs), cfg.Roots.Light)
	fmt.Printf("run: go run ./cmd/asiair-commander --data-dir %s\n", dir)
	fmt.Printf("then log in on :%d with 12345678, select the three %q frames on Tools,\n", cfg.WebPort, wrongTarget)
	fmt.Printf("type %q and press Preview.\n", destTarget)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
