// Command starfield renders the demo's synthetic star fields to FITS files, for
// a look at them outside the demo or a plate-solve check with astap_cli. Catalog
// cones and renders are cached under the user cache dir (see `starfield
// cachedir`); nothing it writes may be committed (Gaia DR3 is CC BY-NC 3.0 IGO).
//
// Usage:
//
//	go run ./tools/starfield/cmd/starfield list
//	go run ./tools/starfield/cmd/starfield cachedir
//	go run ./tools/starfield/cmd/starfield render [-out <dir>] [-png] [target ...]
//
// render writes each named target (every preset when none is named) to <dir>
// under its ASIAIR frame name; -png adds a stretched preview beside each.
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/macourteau/asiair-commander/tools/starfield"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "list":
		for _, t := range starfield.Targets {
			w, h := t.Optics.FOV()
			fmt.Printf("%-16s RA %8.4f Dec %+8.4f  %4.0f mm  %dx%d  %.2fx%.2f deg  G<=%.1f\n",
				t.Name, t.RA, t.Dec, t.Optics.FocalLengthMm, t.Optics.Width(), t.Optics.Height(), w, h, t.MagLimit)
		}
	case "cachedir":
		dir, err := starfield.DefaultCacheDir()
		must(err)
		fmt.Println(dir)
	case "render":
		fs := flag.NewFlagSet("render", flag.ExitOnError)
		out := fs.String("out", ".", "output directory")
		withPNG := fs.Bool("png", false, "also write a stretched PNG preview of each frame")
		must(fs.Parse(os.Args[2:]))
		must(render(logger, *out, *withPNG, fs.Args()))
	default:
		usage()
	}
}

func render(logger *slog.Logger, out string, withPNG bool, names []string) error {
	dir, err := starfield.DefaultCacheDir()
	if err != nil {
		return err
	}
	targets := starfield.Targets
	if len(names) > 0 {
		targets = nil
		for _, n := range names {
			t, ok := starfield.TargetByName(n)
			if !ok {
				return fmt.Errorf("unknown target %q (see `starfield list`)", n)
			}
			targets = append(targets, t)
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	sky := &starfield.Sky{Dir: dir, Log: logger}
	for _, t := range targets {
		f, err := sky.Render(context.Background(), t)
		if err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
		path := filepath.Join(out, starfield.FrameName(t, starfield.ReferenceExposure))
		if err := os.WriteFile(path, f.File, 0o644); err != nil {
			return err
		}
		logger.Info("wrote frame", "path", path)
		if withPNG {
			img, err := starfield.Decode(f.Data, t.Optics.Width(), t.Optics.Height())
			if err != nil {
				return err
			}
			pngPath := strings.TrimSuffix(path, ".fit") + ".png"
			if err := writePNG(pngPath, img); err != nil {
				return err
			}
			logger.Info("wrote preview", "path", pngPath)
		}
	}
	return nil
}

// writePNG stretches img for a look by eye: black at the median, an asinh curve
// up to the brightest pixel.
func writePNG(path string, img starfield.Image) error {
	sorted := slices.Clone(img.Pix)
	slices.Sort(sorted)
	lo := float64(sorted[len(sorted)/2])
	hi := float64(sorted[len(sorted)-1])
	k := 1 / math.Asinh((hi-lo)/50)
	g := image.NewGray(image.Rect(0, 0, img.Width, img.Height))
	for i, v := range img.Pix {
		s := math.Asinh(math.Max(float64(v)-lo, 0)/50) * k
		g.SetGray(i%img.Width, i/img.Width, color.Gray{Y: uint8(math.Min(s, 1) * 255)})
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, g); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: starfield list | cachedir | render [-out <dir>] [-png] [target ...]")
	os.Exit(2)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "starfield:", err)
		os.Exit(1)
	}
}
