// Command seedmosaic seeds a data-dir with a mosaic already filed FLAT — one
// top-level folder per pane — which is the situation the mosaic panel grouping
// feature exists to repair. It is the fixture for verifying the Tools re-file
// panel against real frames rather than synthetic ones.
//
// Usage:
//
//	go run ./tools/seedmosaic <data-dir> <fits-root>
//
// It copies every .fit/.fits under <fits-root> into the seeded light root at the
// path the default light template produces, classifies each through the real
// header-parse path, and upserts it as a verified frame whose dest_path is the
// copy. Mosaic detection is seeded OFF, so the tree looks exactly as it does for
// an operator who shot a mosaic with detection off: Sadr_1-1/ … Sadr_2-5/ as
// siblings. Turning the Settings toggle on and running the Tools re-file
// preview is then the migration under test.
//
// Unlike tools/seedreal it takes EVERY file rather than one per directory, since
// a mosaic's panes all live in one capture folder. The login password is
// "test1234" on port 8787.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/panel"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	ota     = "FRA300"
	session = "Summer2026"
	rigID   = "rig-01"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: seedmosaic <data-dir> <fits-root>")
		os.Exit(1)
	}
	dir, root := os.Args[1], os.Args[2]
	must(os.MkdirAll(dir, 0o700))

	cfg := config.Defaults()
	cfg.WebPort = 8787
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	for _, p := range []string{cfg.Roots.Light, cfg.Roots.Cal, cfg.Roots.Staging, cfg.Roots.Logs} {
		must(os.MkdirAll(p, 0o755))
	}
	// Seeded OFF on purpose: the flat tree below is what an operator who shot a
	// mosaic before the feature existed actually has, and enabling detection is
	// the step under test.
	cfg.MosaicDetection = false
	hash, err := bcrypt.GenerateFromPassword([]byte("test1234"), bcrypt.MinCost)
	must(err)
	cfg.PasswordHash = string(hash)
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	cfg.SessionSecret = base64.StdEncoding.EncodeToString(secret)
	// A managed rig must watch a share and bind to a device, or the config fails
	// validation and the Settings page refuses every save. The host is never
	// contacted — the seeded frames are already archived — but it has to be there.
	cfg.Rigs = []model.RigProfile{{
		ID: rigID, Name: "Mosaic rig", OTA: ota, Session: session,
		Host:    "asiair-mosaic.invalid",
		Managed: true, KeepOriginals: true,
		Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
	}}
	must(config.Save(filepath.Join(dir, "config.yaml"), cfg))

	files := discover(root)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "fatal: no .fit/.fits files under %s\n", root)
		os.Exit(1)
	}

	st, err := store.Open(filepath.Join(dir, "state.db"))
	must(err)
	defer st.Close()

	rdr := header.New()
	ctx := context.Background()
	now := time.Now().Unix()
	profile := cfg.Rigs[0]
	seeded := 0

	for i, src := range files {
		h, herr := rdr.ReadHeader(src)
		if herr != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", filepath.Base(src), herr)
			continue
		}
		cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
		if cls.Type != model.TypeLight {
			continue
		}

		// Route through the real Router with detection off, which is what produced
		// the flat layout in the first place.
		obs := route.ObservingDate(cls.DateObs, time.Local)
		night, nerr := st.AssignNight(ctx, store.NightRequest{
			Target: cls.Target, OTA: ota, ObservingDate: route.ObservingDateKey(obs),
		})
		must(nerr)
		res, rerr := route.Route(cls, profile, route.Options{
			Templates:         cfg.Templates,
			Night:             night,
			ObservingDate:     obs,
			DateFormat:        cfg.DateFormat,
			StripTargetSpaces: cfg.StripTargetSpaces,
			MosaicDetection:   false,
		})
		must(rerr)

		destDir := filepath.Join(cfg.Roots.Light, filepath.FromSlash(res.Dir))
		must(os.MkdirAll(destDir, 0o755))
		dest := filepath.Join(destDir, filepath.Base(src))
		sum, size := copyFile(src, dest)

		f := &model.Frame{
			RigID:       rigID,
			SourceShare: "EMMC Images",
			SourcePath:  "Autorun/" + filepath.Base(src),
			SHA256:      sum,
			Size:        size,
			// The archive side describes the NAS copy this tool just wrote: the re-file
			// the seeded library exists to exercise verifies against these anchors.
			ArchiveSHA256: sum,
			ArchiveSize:   size,
			Mtime:         now - int64((len(files)-i)*600),
			FrameType:     string(cls.Type),
			Target:        cls.Target,
			Filter:        cls.Filter,
			Sensor:        cls.Sensor,
			Binning:       binningToken(cls),
			Night:         int64(night),
			DestPath:      dest,
			State:         model.StateVerified,
			PulledAt:      now - int64((len(files)-i)*600),
		}
		if cls.ExptimePresent {
			f.Exptime = cls.Exptime
		}
		if cls.SetTempPresent {
			f.SetTemp = cls.SetTempC
		}
		f.SetTempAbsent = !cls.SetTempPresent
		if cls.GainPresent {
			f.Gain = cls.Gain
		}
		if cls.OffsetPresent {
			f.Offset = cls.Offset
		}
		if cls.DateObsPresent {
			f.DateObs = cls.DateObs.Format(time.RFC3339Nano)
		}
		// The raw target, matching what recordFiled writes.
		if base, _, row, col, ok := panel.ParsePanel(cls.Target); ok {
			f.PanelBase, f.PanelRow, f.PanelCol = base, row, col
		}
		must(st.UpsertFrame(ctx, f))
		seeded++
		fmt.Printf("  %-12s -> %s\n", cls.Target, strings.TrimPrefix(dest, cfg.Roots.Light+string(os.PathSeparator)))
	}

	fmt.Printf("\nseeded %d frames into %s\n", seeded, cfg.Roots.Light)
	fmt.Printf("run: make demo-up TAGS= DEMO_DATA=%s (make demo-down stops it)\n", dir)
	fmt.Println("then log in on :8787 with test1234, enable mosaic detection in Settings,")
	fmt.Println("and run the Tools re-file preview.")
}

func binningToken(cls model.Classification) string {
	if !cls.BinPresent {
		return ""
	}
	if cls.BinX == cls.BinY {
		return fmt.Sprintf("Bin%d", cls.BinX)
	}
	return fmt.Sprintf("Bin%dx%d", cls.BinX, cls.BinY)
}

// copyFile copies src to dest and returns the copy's sha256 and size, so the
// seeded row's hash describes the file the app will actually find.
func copyFile(src, dest string) (string, int64) {
	in, err := os.Open(src)
	must(err)
	defer in.Close()
	out, err := os.Create(dest)
	must(err)
	defer out.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	must(err)
	must(out.Sync())
	return hex.EncodeToString(h.Sum(nil)), n
}

// discover returns every .fit/.fits under root, sorted. A mosaic's panes all sit
// in one capture folder, so unlike seedreal this takes them all.
func discover(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".fit", ".fits":
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
