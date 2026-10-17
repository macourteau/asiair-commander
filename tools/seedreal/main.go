// Command seedreal seeds a data-dir with frames that point at REAL FITS files on
// disk, so a running server backfills real astrometry and renders real previews for
// UI verification. Unlike tools/seeddemo (synthetic frames), this dogfoods the
// actual header-parse + classify path against a tree of captured subs.
//
// Usage:
//
//	go run ./tools/seedreal <data-dir> <fits-root>
//	SEED_FITS_ROOT=/path/to/subs go run ./tools/seedreal <data-dir>
//
// It walks <fits-root> for .fit/.fits files, takes the first file in each
// containing directory (so a per-filter tree yields one sub per filter), classifies
// each via internal/classify, and upserts it as a verified frame whose dest_path is
// the real file. A single managed rig profile ("rig-01") is written so the Devices
// page renders the seeded frames. The login password is "test1234" on port 8787.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
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
	"github.com/macourteau/asiair-commander/internal/store"
)

// maxFrames bounds how many subs are seeded so a huge capture tree stays a quick
// demo rather than importing thousands of rows.
const maxFrames = 24

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seedreal <data-dir> [<fits-root>]  (or set SEED_FITS_ROOT)")
		os.Exit(1)
	}
	dir := os.Args[1]
	root := os.Getenv("SEED_FITS_ROOT")
	if len(os.Args) >= 3 {
		root = os.Args[2]
	}
	if root == "" {
		fmt.Fprintln(os.Stderr, "fatal: provide a FITS root as arg 2 or SEED_FITS_ROOT")
		os.Exit(1)
	}
	must(os.MkdirAll(dir, 0o700))

	cfg := config.Defaults()
	cfg.WebPort = 8787
	hash, err := bcrypt.GenerateFromPassword([]byte("test1234"), bcrypt.MinCost)
	must(err)
	cfg.PasswordHash = string(hash)
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	cfg.SessionSecret = base64.StdEncoding.EncodeToString(secret)
	// One managed rig so the Devices surface has a card to render the recent transfers.
	cfg.Rigs = []model.RigProfile{{ID: "rig-01", Name: "Seed rig", OTA: "OTA", Managed: true}}
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
	seeded := 0
	for i, path := range files {
		fi, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		var cls model.Classification
		if h, err := rdr.ReadHeader(path); err == nil {
			cls = classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
		}
		ftype := string(cls.Type)
		if ftype == "" || cls.Type == model.TypeUnknown {
			ftype = "UNKNOWN"
		}
		f := &model.Frame{
			RigID:       "rig-01",
			SourceShare: "Astro",
			SourcePath:  strings.TrimPrefix(path, root),
			SHA256:      fmt.Sprintf("%064x", i+1),
			Size:        fi.Size(),
			// The archive side describes the NAS copy; a seeded row's two copies are
			// the same bytes, so both anchors carry the same values.
			ArchiveSHA256: fmt.Sprintf("%064x", i+1),
			ArchiveSize:   fi.Size(),
			Mtime:         fi.ModTime().Unix(),
			FrameType:     ftype,
			Target:        cls.Target,
			Filter:        cls.Filter,
			Sensor:        cls.Sensor,
			Night:         1,
			DestPath:      path,
			State:         model.StateVerified,
			// Stagger pulled_at so the history/feed order is stable and legible.
			PulledAt: now - int64((len(files)-i)*120),
		}
		if cls.ExptimePresent {
			f.Exptime = cls.Exptime
		}
		must(st.UpsertFrame(ctx, f))
		seeded++
	}
	fmt.Printf("seeded %d frames from %s into %s (password: test1234, port 8787)\n", seeded, root, dir)
}

// discover walks root and returns the first .fit/.fits file (lexically) in each
// containing directory, capped at maxFrames — so a per-filter capture tree yields
// one representative sub per filter rather than thousands of near-identical rows.
func discover(root string) []string {
	perDir := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".fit" && ext != ".fits" {
			return nil
		}
		dir := filepath.Dir(path)
		if cur, ok := perDir[dir]; !ok || filepath.Base(path) < filepath.Base(cur) {
			perDir[dir] = path
		}
		return nil
	})
	out := make([]string, 0, len(perDir))
	for _, p := range perDir {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > maxFrames {
		out = out[:maxFrames]
	}
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
