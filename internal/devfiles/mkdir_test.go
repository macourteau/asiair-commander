package devfiles

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
)

func TestMkdir(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddDir(emmc, "Preview", t0)
	if err := fx.svc.Mkdir(ctx, "r1", emmc, "Preview", "Reference frames"); err != nil {
		t.Fatal(err)
	}
	h, _ := f.Open(ctx)
	sh, _ := h.Mount(emmc)
	fi, err := sh.Stat("Preview/Reference frames")
	if err != nil || !fi.IsDir {
		t.Fatalf("stat = %+v, %v, want a folder", fi, err)
	}
	// A missing parent is not_found, after no retry.
	// The message names the missing parent, not the folder that was never made.
	err = fx.svc.Mkdir(ctx, "r1", emmc, "Gone", "x")
	e := wantKind(t, err, ErrNotFound)
	if want := emmc + "/Gone no longer exists on Rig One"; e.Msg != want {
		t.Errorf("message = %q, want %q", e.Msg, want)
	}
}

func TestMkdirExists(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddDir(emmc, "Preview/M42", t0)
	f.AddFile(emmc, "Preview/ref.fit", []byte("x"), t0)
	for _, name := range []string{"M42", "ref.fit"} {
		err := fx.svc.Mkdir(ctx, "r1", emmc, "Preview", name)
		e := wantKind(t, err, ErrExists)
		if want := "A file or folder named " + name + " already exists here."; e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
	}
	if got := fx.opens.Load(); got != 1 {
		t.Errorf("opens = %d, want 1 (exists is not retried)", got)
	}
}

func TestMkdirRefusesBadNames(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	for _, name := range []string{"", "..", "a/b", "trail.", model.MediaMarkerName} {
		err := fx.svc.Mkdir(ctx, "r1", emmc, "", name)
		wantKind(t, err, ErrBadPath)
	}
	wantKind(t, fx.svc.Mkdir(ctx, "r1", emmc, "../x", "ok"), ErrBadPath)
	if got := fx.opens.Load(); got != 0 {
		t.Errorf("opens = %d, want 0 (refused before any device call)", got)
	}
}

func TestLayoutGuardRefusesFlatRoot(t *testing.T) {
	ctx := context.Background()

	listing := func(t *testing.T, f *device.Fake, share string) []device.FileInfo {
		t.Helper()
		s, _ := f.Open(ctx)
		sh, _ := s.Mount(share)
		fis, err := sh.ListDir("")
		if err != nil {
			t.Fatal(err)
		}
		return fis
	}

	t.Run("mkdir of a configured root at a Udisk root is refused while the nested root exists", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(udisk, "ASIAIR/Autorun/Light/a.fit", []byte("frame"), t0)
		before := listing(t, f, udisk)
		for _, name := range []string{"Autorun", "AUTORUN", "autorun"} {
			err := fx.svc.Mkdir(ctx, "r1", udisk, "", name)
			e := wantKind(t, err, ErrLayout)
			if want := "Creating " + name + " here would hide " + udisk + "/ASIAIR/Autorun from Commander's harvester."; e.Msg != want {
				t.Errorf("message = %q, want %q", e.Msg, want)
			}
		}
		if got := f.Mutations(); got != 0 {
			t.Errorf("mutations = %d, want 0", got)
		}
		if after := listing(t, f, udisk); !reflect.DeepEqual(before, after) {
			t.Errorf("share changed: %v -> %v", before, after)
		}
	})

	// Samba resolves share names case-insensitively, so a request naming the
	// share in another case reaches the same Udisk and must meet the same guard.
	// The fake hosts the share under the request's case to stand in for that.
	t.Run("a mis-cased share is guarded too", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		const upper = "UDISK IMAGES"
		f.AddFile(upper, "ASIAIR/Autorun/Light/a.fit", []byte("frame"), t0)
		wantKind(t, fx.svc.Mkdir(ctx, "r1", upper, "", "Autorun"), ErrLayout)
		if got := f.Mutations(); got != 0 {
			t.Errorf("mutations = %d, want 0", got)
		}
	})

	t.Run("a disabled source's root is guarded too", func(t *testing.T) {
		fx := newFixture(t)
		fx.editRig("r1", func(p *model.RigProfile) {
			p.Sources = []model.Source{{Share: udisk, Roots: []string{"Plan"}, Disabled: true}}
		})
		fx.dev(ip1).AddDir(udisk, "ASIAIR/Plan", t0)
		wantKind(t, fx.svc.Mkdir(ctx, "r1", udisk, "", "Plan"), ErrLayout)
	})

	t.Run("a flat root is refused while ASIAIR holds any other configured root", func(t *testing.T) {
		fx := newFixture(t)
		fx.editRig("r1", func(p *model.RigProfile) {
			p.Sources = []model.Source{{Share: udisk, Roots: []string{"Plan", "Autorun"}}}
		})
		f := fx.dev(ip1)
		f.AddFile(udisk, "ASIAIR/Autorun/Light/a.fit", []byte("frame"), t0)
		before := listing(t, f, udisk)
		err := fx.svc.Mkdir(ctx, "r1", udisk, "", "Plan")
		e := wantKind(t, err, ErrLayout)
		if want := "Creating Plan here would hide " + udisk + "/ASIAIR/Autorun from Commander's harvester."; e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
		if got := f.Mutations(); got != 0 {
			t.Errorf("mutations = %d, want 0", got)
		}
		if after := listing(t, f, udisk); !reflect.DeepEqual(before, after) {
			t.Errorf("share changed: %v -> %v", before, after)
		}
	})

	t.Run("allowed while ASIAIR holds no configured root", func(t *testing.T) {
		fx := newFixture(t)
		fx.editRig("r1", func(p *model.RigProfile) {
			p.Sources = []model.Source{{Share: udisk, Roots: []string{"Plan", "Autorun"}}}
		})
		fx.dev(ip1).AddFile(udisk, "ASIAIR/Other/a.fit", []byte("frame"), t0)
		if err := fx.svc.Mkdir(ctx, "r1", udisk, "", "Plan"); err != nil {
			t.Errorf("Mkdir: %v", err)
		}
	})

	t.Run("allowed on a fixed share although its ASIAIR holds a root", func(t *testing.T) {
		fx := newFixture(t)
		fx.editRig("r1", func(p *model.RigProfile) {
			p.Sources = []model.Source{{Share: emmc, Roots: []string{"Plan", "Autorun"}}}
		})
		fx.dev(ip1).AddFile(emmc, "ASIAIR/Autorun/a.fit", []byte("frame"), t0)
		if err := fx.svc.Mkdir(ctx, "r1", emmc, "", "Plan"); err != nil {
			t.Errorf("Mkdir: %v", err)
		}
	})

	t.Run("allowed where the harvester cannot be misled", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(udisk, "ASIAIR/Autorun/Light/a.fit", []byte("frame"), t0)
		f.AddFile(emmc, "ASIAIR/Autorun/x", []byte("x"), t0)
		cases := []struct{ share, dir, name string }{
			{udisk, "", "Preview"},       // not a configured root
			{udisk, "Backup", "Autorun"}, // not at the share root
			{emmc, "", "Autorun"},        // a fixed share
		}
		f.AddDir(udisk, "Backup", t0)
		for _, c := range cases {
			if err := fx.svc.Mkdir(ctx, "r1", c.share, c.dir, c.name); err != nil {
				t.Errorf("Mkdir(%s, %q, %s): %v", c.share, c.dir, c.name, err)
			}
		}

		// A Udisk with no nested root keeps the flat layout available.
		g := fx.dev(ip2)
		g.AddDir(udisk, "Other", t0)
		if err := fx.svc.Mkdir(ctx, "r2", udisk, "", "Autorun"); err != nil {
			t.Errorf("Mkdir with no nested root: %v", err)
		}
	})

	t.Run("a failing guard check fails the mkdir", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddDir(udisk, "ASIAIR", t0)
		f.FailNext(device.FakeOpStat, 1, &os.PathError{Op: "stat", Err: os.ErrPermission})
		wantKind(t, fx.svc.Mkdir(ctx, "r1", udisk, "", "Autorun"), ErrDenied)
		if got := f.Mutations(); got != 0 {
			t.Errorf("mutations = %d, want 0", got)
		}
	})
}
