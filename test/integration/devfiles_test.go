//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
)

// The device file browser's seam primitives against real Samba: exclusive
// create, mkdir, the never-replacing rename, chtimes, and the two delete
// refusals the tool reports (a non-empty folder, a file another session holds
// open). Each test works in its own scratch folder at the share root, so it
// shares nothing with the harvester fixtures or with other tests.

// devfilesMount opens a fresh session on the container and mounts the share,
// closing the session at cleanup.
func devfilesMount(t *testing.T) device.Share {
	t.Helper()
	sess, err := newDevice(t).Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	sh, err := sess.Mount(shareName)
	if err != nil {
		t.Fatalf("Mount(%q): %v", shareName, err)
	}
	return sh
}

// devfilesScratch makes a uniquely named folder at the share root and removes
// it, with everything left in it, at cleanup through a session of its own.
func devfilesScratch(t *testing.T, sh device.Share) string {
	t.Helper()
	dir := fmt.Sprintf("DevFiles-%s-%d", t.Name(), time.Now().UnixNano())
	if err := sh.Mkdir(dir); err != nil {
		t.Fatalf("Mkdir(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		cleanup := devfilesMount(t)
		devfilesRemoveAll(t, cleanup, dir)
	})
	return dir
}

func devfilesRemoveAll(t *testing.T, sh device.Share, dir string) {
	entries, err := sh.ListDir(dir)
	if err != nil {
		t.Errorf("cleanup: ListDir(%q): %v", dir, err)
		return
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name)
		if e.IsDir {
			devfilesRemoveAll(t, sh, p)
			continue
		}
		if err := sh.Delete(p); err != nil {
			t.Errorf("cleanup: Delete(%q): %v", p, err)
		}
	}
	if err := sh.Delete(dir); err != nil {
		t.Errorf("cleanup: Delete(%q): %v", dir, err)
	}
}

func devfilesWrite(t *testing.T, sh device.Share, p string, data []byte, chunk int) {
	t.Helper()
	w, err := sh.CreateNew(p)
	if err != nil {
		t.Fatalf("CreateNew(%q): %v", p, err)
	}
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		if _, err := w.Write(data[off:end]); err != nil {
			w.Close()
			t.Fatalf("Write(%q) at %d: %v", p, off, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close(%q): %v", p, err)
	}
}

func devfilesRead(t *testing.T, sh device.Share, p string) []byte {
	t.Helper()
	rc, err := sh.OpenReader(p)
	if err != nil {
		t.Fatalf("OpenReader(%q): %v", p, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", p, err)
	}
	return data
}

// CreateNew writes a multi-chunk file through the idle-deadline writer, and an
// exclusive create onto an existing name, or a case variant of it, is refused
// with IsExist and leaves that file byte-identical.
func TestDevfilesCreateNewExclusive(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)

	data := make([]byte, 3<<20+12345)
	for i := range data {
		data[i] = byte(i*31 + i>>11)
	}
	p := path.Join(dir, "frame.fit")
	devfilesWrite(t, sh, p, data, 256<<10)
	if got := devfilesRead(t, sh, p); !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes, differing from the %d written", len(got), len(data))
	}

	for _, name := range []string{"frame.fit", "FRAME.FIT"} {
		w, err := sh.CreateNew(path.Join(dir, name))
		if err == nil {
			w.Close()
			t.Fatalf("CreateNew(%q) onto an existing file succeeded", name)
		}
		if !device.IsExist(err) {
			t.Fatalf("CreateNew(%q) err = %v, want IsExist", name, err)
		}
	}
	if got := devfilesRead(t, sh, p); !bytes.Equal(got, data) {
		t.Fatal("a refused CreateNew changed the existing file")
	}
}

func TestDevfilesMkdir(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)

	sub := path.Join(dir, "Night1")
	if err := sh.Mkdir(sub); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	fi, err := sh.Stat(sub)
	if err != nil || !fi.IsDir {
		t.Fatalf("Stat(%q) = %+v, %v; want a folder", sub, fi, err)
	}
	if err := sh.Mkdir(sub); !device.IsExist(err) {
		t.Fatalf("Mkdir onto an existing folder err = %v, want IsExist", err)
	}
	devfilesWrite(t, sh, path.Join(dir, "a.fit"), []byte("a"), 1)
	if err := sh.Mkdir(path.Join(dir, "a.fit")); !device.IsExist(err) {
		t.Fatalf("Mkdir onto an existing file err = %v, want IsExist", err)
	}
}

// Rename never replaces: a collision, also through a case variant, reports
// IsExist with the target and the source both unchanged.
func TestDevfilesRenameNeverReplaces(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)

	tmp, dst := path.Join(dir, ".tmp-copy"), path.Join(dir, "ref.fit")
	devfilesWrite(t, sh, tmp, []byte("new bytes"), 4)
	devfilesWrite(t, sh, dst, []byte("old bytes"), 4)

	for _, target := range []string{dst, path.Join(dir, "REF.FIT")} {
		if err := sh.Rename(tmp, target); !device.IsExist(err) {
			t.Fatalf("Rename onto %q err = %v, want IsExist", target, err)
		}
		if got := devfilesRead(t, sh, dst); string(got) != "old bytes" {
			t.Fatalf("the rename target was replaced: %q", got)
		}
		if got := devfilesRead(t, sh, tmp); string(got) != "new bytes" {
			t.Fatalf("the rename source changed: %q", got)
		}
	}

	final := path.Join(dir, "final.fit")
	if err := sh.Rename(tmp, final); err != nil {
		t.Fatalf("Rename onto a free name: %v", err)
	}
	if _, err := sh.Stat(tmp); !device.IsNotFound(err) {
		t.Fatalf("source still present after rename: %v", err)
	}
	if got := devfilesRead(t, sh, final); string(got) != "new bytes" {
		t.Fatalf("renamed content = %q", got)
	}
}

// Chtimes sets the mtime a fresh session's listing reads back.
func TestDevfilesChtimes(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)

	p := path.Join(dir, "a.fit")
	devfilesWrite(t, sh, p, []byte("a"), 1)
	want := time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)
	if err := sh.Chtimes(p, want, want); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	fresh := devfilesMount(t)
	entries, err := fresh.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.fit" {
		t.Fatalf("ListDir(%q) = %+v", dir, entries)
	}
	if !entries[0].Mtime.Equal(want) {
		t.Fatalf("listed mtime = %v, want %v", entries[0].Mtime, want)
	}
}

// A non-empty folder delete is refused with IsDirNotEmpty and leaves the
// folder and its contents in place. The dockerized Samba acknowledged a
// delete-on-close of a non-empty folder without removing it, so before
// smb1client sent SMB_COM_DELETE_DIRECTORY this test saw a nil error.
func TestDevfilesDeleteNonEmptyRefused(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)

	sub := path.Join(dir, "Night1")
	if err := sh.Mkdir(sub); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	child := path.Join(sub, "a.fit")
	devfilesWrite(t, sh, child, []byte("keep me"), 7)

	err := sh.Delete(sub)
	if !device.IsDirNotEmpty(err) {
		t.Fatalf("Delete of a non-empty folder err = %v, want IsDirNotEmpty", err)
	}
	if device.IsInUse(err) || device.IsNotFound(err) {
		t.Fatalf("non-empty refusal misclassified: %v", err)
	}
	if fi, err := sh.Stat(sub); err != nil || !fi.IsDir {
		t.Fatalf("refused folder: Stat = %+v, %v", fi, err)
	}
	if got := devfilesRead(t, sh, child); string(got) != "keep me" {
		t.Fatalf("refused folder's file = %q", got)
	}

	if err := sh.Delete(child); err != nil {
		t.Fatalf("Delete file: %v", err)
	}
	if err := sh.Delete(sub); err != nil {
		t.Fatalf("Delete of the emptied folder: %v", err)
	}
	if _, err := sh.Stat(sub); !device.IsNotFound(err) {
		t.Fatalf("emptied folder still present: %v", err)
	}
}

// A file another session holds open cannot be deleted: the sharing violation
// reports IsInUse (and, as the library documents, IsPermission too), and the
// file stays.
func TestDevfilesDeleteHeldOpenInUse(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)

	p := path.Join(dir, "held.fit")
	devfilesWrite(t, sh, p, []byte("held"), 4)

	holder := devfilesMount(t)
	rc, err := holder.OpenReader(p)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	err = sh.Delete(p)
	if !device.IsInUse(err) || !device.IsPermission(err) {
		rc.Close()
		t.Fatalf("Delete of a held-open file err = %v, want IsInUse and IsPermission", err)
	}
	if device.IsDirNotEmpty(err) {
		t.Fatalf("held-open refusal misclassified as not-empty: %v", err)
	}
	if got := devfilesRead(t, sh, p); string(got) != "held" {
		t.Fatalf("held file = %q", got)
	}

	rc.Close()
	if err := sh.Delete(p); err != nil {
		t.Fatalf("Delete after the holder closed: %v", err)
	}
}
