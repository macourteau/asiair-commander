//go:build integration

package integration

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
)

// Service.Rename against real Samba: the device's never-replacing rename of a
// file or a folder in place, and the refusals the configuration decides
// before any device call. Each test works in its own scratch folder at the
// share root, and any capture source it configures is rooted inside it.

// TestDevfilesServiceRename renames onto a free name, onto a taken one (also
// through a case variant), a folder with its subtree, and a source not listed
// under exactly its name.
func TestDevfilesServiceRename(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	svc, ctx := devfilesService(t, nil), context.Background()
	devfilesWrite(t, sh, path.Join(dir, "a.fit"), []byte("frame a"), 1<<20)
	devfilesWrite(t, sh, path.Join(dir, "c.fit"), []byte("frame c"), 1<<20)

	t.Run("FreeName", func(t *testing.T) {
		if err := svc.Rename(ctx, "b", shareName, dir, "a.fit", "b.fit"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		check := devfilesMount(t)
		if got := devfilesRead(t, check, path.Join(dir, "b.fit")); string(got) != "frame a" {
			t.Fatalf("b.fit = %q", got)
		}
		if _, err := check.Stat(path.Join(dir, "a.fit")); !device.IsNotFound(err) {
			t.Fatalf("a.fit still there: Stat err = %v", err)
		}
	})

	t.Run("TakenName", func(t *testing.T) {
		for _, target := range []string{"c.fit", "C.FIT"} {
			err := svc.Rename(ctx, "b", shareName, dir, "b.fit", target)
			if !errors.Is(err, devfiles.ErrExists) || !strings.Contains(err.Error(), "Nothing was replaced.") {
				t.Fatalf("Rename onto %q err = %v, want exists", target, err)
			}
			check := devfilesMount(t)
			if got := devfilesRead(t, check, path.Join(dir, "c.fit")); string(got) != "frame c" {
				t.Fatalf("onto %q: the taken name was replaced: %q", target, got)
			}
			if got := devfilesRead(t, check, path.Join(dir, "b.fit")); string(got) != "frame a" {
				t.Fatalf("onto %q: the source changed: %q", target, got)
			}
		}
	})

	t.Run("FolderSubtree", func(t *testing.T) {
		night := path.Join(dir, "Night1")
		for _, d := range []string{night, path.Join(night, "M42")} {
			if err := sh.Mkdir(d); err != nil {
				t.Fatalf("Mkdir(%q): %v", d, err)
			}
		}
		devfilesWrite(t, sh, path.Join(night, "M42", "x.fit"), []byte("deep"), 1<<20)
		devfilesWrite(t, sh, path.Join(night, "y.fit"), []byte("shallow"), 1<<20)

		if err := svc.Rename(ctx, "b", shareName, dir, "Night1", "Night 2"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		check := devfilesMount(t)
		if _, err := check.Stat(night); !device.IsNotFound(err) {
			t.Fatalf("Night1 still there: Stat err = %v", err)
		}
		moved := path.Join(dir, "Night 2")
		if got := devfilesRead(t, check, path.Join(moved, "M42", "x.fit")); string(got) != "deep" {
			t.Fatalf("Night 2/M42/x.fit = %q", got)
		}
		if got := devfilesRead(t, check, path.Join(moved, "y.fit")); string(got) != "shallow" {
			t.Fatalf("Night 2/y.fit = %q", got)
		}
	})

	t.Run("NotListedExactly", func(t *testing.T) {
		// Samba would resolve B.FIT to b.fit; the service renames only the
		// entry listed under exactly the name it was given.
		for _, name := range []string{"B.FIT", "gone.fit"} {
			err := svc.Rename(ctx, "b", shareName, dir, name, "d.fit")
			if !errors.Is(err, devfiles.ErrNotFound) {
				t.Fatalf("Rename(%q) err = %v, want not_found", name, err)
			}
		}
		check := devfilesMount(t)
		if got := devfilesRead(t, check, path.Join(dir, "b.fit")); string(got) != "frame a" {
			t.Fatalf("b.fit = %q", got)
		}
		if _, err := check.Stat(path.Join(dir, "d.fit")); !device.IsNotFound(err) {
			t.Fatalf("d.fit appeared: Stat err = %v", err)
		}
	})
}

// TestDevfilesRenameRefusals checks every refusal decided from the request
// and the configuration leaves the device untouched: inside a watched root
// (R1), a folder that is or holds a root and a new name that would be or hold
// one (R2), a turned-off source's root counting as much as an enabled one's,
// marker names (R4), an unchanged name and a crafted segment.
func TestDevfilesRenameRefusals(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	auto := path.Join(dir, "Autorun")
	for _, d := range []string{auto, path.Join(dir, "Off")} {
		if err := sh.Mkdir(d); err != nil {
			t.Fatalf("Mkdir(%q): %v", d, err)
		}
	}
	devfilesWrite(t, sh, path.Join(auto, "f.fit"), []byte("frame"), 1<<20)
	devfilesWrite(t, sh, path.Join(dir, "Off", "g.fit"), []byte("frame"), 1<<20)
	devfilesWrite(t, sh, path.Join(dir, "free.fit"), []byte("free"), 1<<20)

	env := devfilesServiceWith(t, devfilesOpts{sources: []model.Source{
		{Share: shareName, Roots: []string{auto, path.Join(dir, "Plan", "Deep")}},
		{Share: shareName, Roots: []string{path.Join(dir, "Off")}, Disabled: true},
	}})
	svc, ctx := env.svc, context.Background()

	snapshot := func() map[string]map[string]int64 {
		check := devfilesMount(t)
		return map[string]map[string]int64{
			dir: listNames(t, check, dir), auto: listNames(t, check, auto),
			path.Join(dir, "Off"): listNames(t, check, path.Join(dir, "Off")),
		}
	}
	before := snapshot()

	cases := []struct {
		name               string
		dir, from, newName string
		want               error
		says               string
	}{
		{"R1 a frame in a watched root", auto, "f.fit", "g.fit", devfiles.ErrWatched, "archives this folder by name"},
		{"R1 a turned-off source's frame", path.Join(dir, "Off"), "g.fit", "h.fit", devfiles.ErrWatched, "turned off"},
		{"R2 the root itself", dir, "Autorun", "Autorun2", devfiles.ErrWatched, "a folder Commander depends on"},
		{"R2 the root, other case", dir, "Autorun", "autorun", devfiles.ErrWatched, "a folder Commander depends on"},
		{"R2 a turned-off root", dir, "Off", "On", devfiles.ErrWatched, "a folder Commander depends on"},
		{"R2 a folder holding a root", "", dir, dir + "-moved", devfiles.ErrWatched, "a folder Commander depends on"},
		{"R2 a new name that is a root", dir, "free.fit", "AUTORUN", devfiles.ErrWatched, "would be, or would hold"},
		{"R2 a new name that holds a root", dir, "free.fit", "Plan", devfiles.ErrWatched, "would be, or would hold"},
		{"R4 a marker as the new name", dir, "free.fit", model.MediaMarkerName, devfiles.ErrBadPath, ""},
		{"R4 a marker as the old name", dir, model.IdentityMarkerName, "x.fit", devfiles.ErrBadPath, ""},
		{"an unchanged name", dir, "free.fit", "free.fit", devfiles.ErrBadPath, "already its name"},
		{"a trailing dot", dir, "free.fit", "free.", devfiles.ErrBadPath, ""},
		{"a wildcard in the old name", dir, "free*", "x.fit", devfiles.ErrBadPath, ""},
		{"a trailing dot in the folder", dir + ".", "free.fit", "x.fit", devfiles.ErrBadPath, ""},
	}
	for _, c := range cases {
		err := svc.Rename(ctx, "b", shareName, c.dir, c.from, c.newName)
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: Rename(%q, %q → %q) err = %v, want %v saying %q",
				c.name, c.dir, c.from, c.newName, err, c.want, c.says)
		}
	}
	after := snapshot()
	for d, names := range before {
		if !sameListing(names, after[d]) {
			t.Errorf("refused renames changed %s: %v → %v", d, names, after[d])
		}
	}

	// Outside every root the same configuration lets a rename through.
	if err := svc.Rename(ctx, "b", shareName, dir, "free.fit", "still-free.fit"); err != nil {
		t.Fatalf("Rename outside the roots: %v", err)
	}
}

// TestDevfilesRenameCaseOnly records what the real server answers to a
// case-only rename (ref.fit → REF.FIT), which the device's never-replacing
// rename may refuse as a collision with itself or carry out, and asserts only
// that the file is never lost: exactly one entry under either spelling, with
// its bytes.
func TestDevfilesRenameCaseOnly(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	svc := devfilesService(t, nil)
	devfilesWrite(t, sh, path.Join(dir, "ref.fit"), []byte("reference"), 1<<20)

	err := svc.Rename(context.Background(), "b", shareName, dir, "ref.fit", "REF.FIT")
	t.Logf("case-only rename answered: %v", err)
	if err != nil && !errors.Is(err, devfiles.ErrExists) {
		t.Errorf("case-only rename err = %v, want success or exists", err)
	}

	check := devfilesMount(t)
	names := listNames(t, check, dir)
	if len(names) != 1 {
		t.Fatalf("folder after a case-only rename = %v, want one entry", names)
	}
	for name := range names {
		t.Logf("the file is listed as %q", name)
		if !strings.EqualFold(name, "ref.fit") {
			t.Fatalf("the file is listed as %q", name)
		}
		if got := devfilesRead(t, check, path.Join(dir, name)); string(got) != "reference" {
			t.Fatalf("%s = %q", name, got)
		}
	}
}
