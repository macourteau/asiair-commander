package device

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	smb1 "github.com/macourteau/smb1client"

	"github.com/macourteau/asiair-commander/internal/clock"
)

const wShare = "EMMC Images"

func readAll(t *testing.T, sh Share, p string) string {
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
	return string(data)
}

func listNames(t *testing.T, sh Share, dir string) []string {
	t.Helper()
	entries, err := sh.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name
		if e.IsDir {
			n += "/"
		}
		out = append(out, n)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CreateNew never touches an existing name, whatever answers to it, and a
// fresh file collects every Write and carries the device clock as its mtime.
func TestFakeCreateNewExclusive(t *testing.T) {
	f := NewFake()
	devNow := time.Date(2019, 2, 14, 12, 0, 0, 0, time.UTC)
	f.SetServerTime(clock.ServerTime{Time: devNow})
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.AddFile(wShare, "Preview/keep.fit", []byte("original"), old)
	f.AddFile(wShare, "Preview/sub/deep.fit", []byte("x"), old)
	f.SetMarker(wShare, "Preview/.marker", []byte("m"))
	sh := mustMount(t, mustOpen(t, f), wShare)

	for _, p := range []string{"Preview/keep.fit", "Preview/sub", "Preview", "Preview/.marker"} {
		w, err := sh.CreateNew(p)
		if !IsExist(err) {
			t.Fatalf("CreateNew(%q) err = %v (writer %v), want IsExist", p, err, w)
		}
	}
	if got := readAll(t, sh, "Preview/keep.fit"); got != "original" {
		t.Fatalf("existing file changed to %q", got)
	}
	if fi, _ := sh.Stat("Preview/keep.fit"); !fi.Mtime.Equal(old) {
		t.Fatalf("existing file mtime changed to %v", fi.Mtime)
	}

	if _, err := sh.CreateNew("Missing/new.fit"); !IsNotFound(err) {
		t.Fatalf("CreateNew under a missing folder err = %v, want not-found", err)
	}
	if _, err := sh.CreateNew("Preview/keep.fit/child"); !IsNotFound(err) {
		t.Fatalf("CreateNew under a file err = %v, want not-found", err)
	}

	before := f.Mutations()
	w, err := sh.CreateNew("Preview/new.fit")
	if err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	// The name is taken from the moment of creation, not from Close.
	if _, err := sh.CreateNew("Preview/new.fit"); !IsExist(err) {
		t.Fatalf("second CreateNew while the first is open err = %v, want IsExist", err)
	}
	for _, chunk := range []string{"abc", "def", "g"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after Close err = %v, want os.ErrClosed", err)
	}
	if got := readAll(t, sh, "Preview/new.fit"); got != "abcdefg" {
		t.Fatalf("written content = %q", got)
	}
	fi, err := sh.Stat("Preview/new.fit")
	if err != nil || fi.Size != 7 || !fi.Mtime.Equal(devNow) {
		t.Fatalf("Stat = %+v, %v; want size 7, mtime %v", fi, err, devNow)
	}
	if got := f.Mutations() - before; got != 2 {
		t.Fatalf("Mutations grew by %d, want 2 (two CreateNew calls)", got)
	}

	injected := errors.New("create refused")
	f.FailCreateNew(wShare, injected)
	if _, err := sh.CreateNew("Preview/other.fit"); !errors.Is(err, injected) {
		t.Fatalf("FailCreateNew: err = %v", err)
	}
	if _, err := sh.Stat("Preview/other.fit"); !IsNotFound(err) {
		t.Fatalf("a failed CreateNew left an entry: %v", err)
	}
}

func TestFakeMkdirExist(t *testing.T) {
	f := NewFake()
	devNow := time.Date(2019, 2, 14, 12, 0, 0, 0, time.UTC)
	f.SetServerTime(clock.ServerTime{Time: devNow})
	f.AddFile(wShare, "Preview/a.fit", []byte("a"), time.Now())
	f.SetMarker(wShare, "ASIAIR/.marker", []byte("m"))
	sh := mustMount(t, mustOpen(t, f), wShare)

	before := f.Mutations()
	if err := sh.Mkdir("Preview/Night1"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if got, want := listNames(t, sh, "Preview"), []string{"Night1/", "a.fit"}; !equalStrings(got, want) {
		t.Fatalf("ListDir = %v, want %v", got, want)
	}
	if fi, err := sh.Stat("Preview/Night1"); err != nil || !fi.IsDir || !fi.Mtime.Equal(devNow) {
		t.Fatalf("Stat new folder = %+v, %v", fi, err)
	}
	if entries := listNames(t, sh, "Preview/Night1"); len(entries) != 0 {
		t.Fatalf("new folder lists %v, want empty", entries)
	}

	// An explicit folder, an implicit one, a file and a folder holding only a
	// marker all occupy their name.
	for _, p := range []string{"Preview/Night1", "Preview", "Preview/a.fit", "ASIAIR"} {
		if err := sh.Mkdir(p); !IsExist(err) {
			t.Fatalf("Mkdir(%q) err = %v, want IsExist", p, err)
		}
	}
	if err := sh.Mkdir("Missing/Night1"); !IsNotFound(err) {
		t.Fatalf("Mkdir under a missing folder err = %v, want not-found", err)
	}
	if got := f.Mutations() - before; got != 6 {
		t.Fatalf("Mutations grew by %d, want 6", got)
	}

	injected := errors.New("mkdir refused")
	f.FailMkdir(wShare, injected)
	if err := sh.Mkdir("Preview/Night2"); !errors.Is(err, injected) {
		t.Fatalf("FailMkdir: err = %v", err)
	}
	if _, err := sh.Stat("Preview/Night2"); !IsNotFound(err) {
		t.Fatalf("a failed Mkdir left an entry: %v", err)
	}
}

// Rename moves files and folders but never replaces an existing target: the
// collision leaves both sides exactly as they were.
func TestFakeRenameNeverReplaces(t *testing.T) {
	f := NewFake()
	src := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	dst := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	f.AddFile(wShare, "Preview/.tmp-1", []byte("new bytes"), src)
	f.AddFile(wShare, "Preview/target.fit", []byte("old bytes"), dst)
	sh := mustMount(t, mustOpen(t, f), wShare)

	before := f.Mutations()
	err := sh.Rename("Preview/.tmp-1", "Preview/target.fit")
	if !IsExist(err) {
		t.Fatalf("Rename onto an existing file err = %v, want IsExist", err)
	}
	var le *os.LinkError
	if !errors.As(err, &le) {
		t.Fatalf("collision err %T is not an *os.LinkError, the library's shape", err)
	}
	if got := readAll(t, sh, "Preview/target.fit"); got != "old bytes" {
		t.Fatalf("target replaced: %q", got)
	}
	if got := readAll(t, sh, "Preview/.tmp-1"); got != "new bytes" {
		t.Fatalf("source changed: %q", got)
	}
	if err := sh.Rename("Preview/.tmp-1", "Preview"); !IsExist(err) {
		t.Fatalf("Rename onto an implicit folder err = %v, want IsExist", err)
	}

	if err := sh.Rename("Preview/.tmp-1", "Preview/final.fit"); err != nil {
		t.Fatalf("Rename onto a free name: %v", err)
	}
	if _, err := sh.Stat("Preview/.tmp-1"); !IsNotFound(err) {
		t.Fatalf("source still present after rename: %v", err)
	}
	fi, err := sh.Stat("Preview/final.fit")
	if err != nil || !fi.Mtime.Equal(src) || readAll(t, sh, "Preview/final.fit") != "new bytes" {
		t.Fatalf("renamed file = %+v, %v", fi, err)
	}

	if err := sh.Rename("Preview/gone", "Preview/x"); !IsNotFound(err) {
		t.Fatalf("Rename of a missing source err = %v, want not-found", err)
	}
	if err := sh.Rename("Preview/final.fit", "Missing/x"); !IsNotFound(err) {
		t.Fatalf("Rename into a missing folder err = %v, want not-found", err)
	}

	// A folder moves with everything below it, markers included.
	f.SetMarker(wShare, "Preview/.marker", []byte("m"))
	if err := sh.Rename("Preview", "Moved"); err != nil {
		t.Fatalf("Rename folder: %v", err)
	}
	if got, want := listNames(t, sh, "Moved"), []string{"final.fit", "target.fit"}; !equalStrings(got, want) {
		t.Fatalf("moved folder lists %v, want %v", got, want)
	}
	if _, err := sh.ReadMarker("Moved/.marker"); err != nil {
		t.Fatalf("marker did not move with its folder: %v", err)
	}
	if _, err := sh.ListDir("Preview"); !IsNotFound(err) {
		t.Fatalf("old folder still lists: %v", err)
	}
	if got := f.Mutations() - before; got != 6 {
		t.Fatalf("Mutations grew by %d, want 6", got)
	}

	injected := errors.New("rename refused")
	f.FailRename(wShare, injected)
	if err := sh.Rename("Moved/final.fit", "Moved/other.fit"); !errors.Is(err, injected) {
		t.Fatalf("FailRename: err = %v", err)
	}
	if _, err := sh.Stat("Moved/final.fit"); err != nil {
		t.Fatalf("a failed Rename moved the file: %v", err)
	}
}

func TestFakeChtimes(t *testing.T) {
	f := NewFake()
	f.AddFile(wShare, "Preview/a.fit", []byte("a"), time.Date(2019, 2, 14, 0, 0, 0, 0, time.UTC))
	sh := mustMount(t, mustOpen(t, f), wShare)

	want := time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)
	before := f.Mutations()
	if err := sh.Chtimes("Preview/a.fit", want, want); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if fi, _ := sh.Stat("Preview/a.fit"); !fi.Mtime.Equal(want) {
		t.Fatalf("Stat mtime = %v, want %v", fi.Mtime, want)
	}
	entries, _ := sh.ListDir("Preview")
	if len(entries) != 1 || !entries[0].Mtime.Equal(want) {
		t.Fatalf("ListDir = %+v, want mtime %v", entries, want)
	}
	// A zero mtime means "leave it", as SMB encodes it.
	if err := sh.Chtimes("Preview/a.fit", time.Time{}, time.Time{}); err != nil {
		t.Fatalf("Chtimes zero: %v", err)
	}
	if fi, _ := sh.Stat("Preview/a.fit"); !fi.Mtime.Equal(want) {
		t.Fatalf("zero Chtimes changed mtime to %v", fi.Mtime)
	}
	// An implicit folder takes a time too.
	if err := sh.Chtimes("Preview", want, want); err != nil {
		t.Fatalf("Chtimes folder: %v", err)
	}
	if fi, _ := sh.Stat("Preview"); !fi.IsDir || !fi.Mtime.Equal(want) {
		t.Fatalf("folder Stat = %+v", fi)
	}
	if err := sh.Chtimes("Preview/missing.fit", want, want); !IsNotFound(err) {
		t.Fatalf("Chtimes of a missing file err = %v, want not-found", err)
	}
	if got := f.Mutations() - before; got != 4 {
		t.Fatalf("Mutations grew by %d, want 4", got)
	}

	injected := errors.New("chtimes refused")
	f.FailChtimes(wShare, injected)
	later := want.Add(time.Hour)
	if err := sh.Chtimes("Preview/a.fit", later, later); !errors.Is(err, injected) {
		t.Fatalf("FailChtimes: err = %v", err)
	}
	if fi, _ := sh.Stat("Preview/a.fit"); !fi.Mtime.Equal(want) {
		t.Fatalf("a failed Chtimes changed mtime to %v", fi.Mtime)
	}
}

// A folder delete is refused while anything — a marker included — is below
// it, in the shape smb1client returns, and the folder survives.
func TestFakeDeleteRefusesNonEmptyDir(t *testing.T) {
	f := NewFake()
	now := time.Now()
	f.AddDir(wShare, "Explicit", now)
	f.AddFile(wShare, "Explicit/a.fit", []byte("a"), now)
	f.AddFile(wShare, "Implicit/b.fit", []byte("b"), now)
	f.AddDir(wShare, "MarkerOnly", now)
	f.SetMarker(wShare, "MarkerOnly/.marker", []byte("m"))
	f.AddDir(wShare, "Empty", now)
	sh := mustMount(t, mustOpen(t, f), wShare)

	for _, dir := range []string{"Explicit", "Implicit", "MarkerOnly"} {
		err := sh.Delete(dir)
		if !IsDirNotEmpty(err) {
			t.Fatalf("Delete(%q) err = %v, want IsDirNotEmpty", dir, err)
		}
		var pe *os.PathError
		var re *smb1.ResponseError
		if !errors.As(err, &pe) || !errors.As(pe.Err, &re) || re.Code != 0xC0000101 {
			t.Fatalf("Delete(%q) err %#v is not *os.PathError wrapping ResponseError 0xC0000101", dir, err)
		}
		if _, err := sh.Stat(dir); err != nil {
			t.Fatalf("refused folder %q is gone: %v", dir, err)
		}
	}
	if _, err := sh.ReadMarker("MarkerOnly/.marker"); err != nil {
		t.Fatalf("marker removed by a refused delete: %v", err)
	}

	if err := sh.Delete("Empty"); err != nil {
		t.Fatalf("Delete of an empty folder: %v", err)
	}
	if err := sh.Delete("Explicit/a.fit"); err != nil {
		t.Fatalf("Delete file: %v", err)
	}
	if err := sh.Delete("Explicit"); err != nil {
		t.Fatalf("Delete of an emptied folder: %v", err)
	}
	if got := listNames(t, sh, ""); !equalStrings(got, []string{"Implicit/", "MarkerOnly/"}) {
		t.Fatalf("root lists %v", got)
	}
}

// FailNext fails exactly n calls of one operation, then lets the next through;
// the sharing-violation builders carry the library's shape, so IsInUse sees
// them where IsPermission alone would misread them as access denied.
func TestFakeFailNextCounts(t *testing.T) {
	f := NewFake()
	now := time.Now()
	f.AddFile(wShare, "held.fit", []byte("h"), now)
	f.AddFile(wShare, "src.fit", []byte("s"), now)
	sh := mustMount(t, mustOpen(t, f), wShare)

	f.FailNext(FakeOpDelete, 2, SharingViolationError("remove", "held.fit"))
	for i := 0; i < 2; i++ {
		err := sh.Delete("held.fit")
		if !IsInUse(err) || !IsPermission(err) {
			t.Fatalf("attempt %d: err = %v, want IsInUse and IsPermission", i, err)
		}
		if _, err := sh.Stat("held.fit"); err != nil {
			t.Fatalf("attempt %d removed the file: %v", i, err)
		}
	}
	if err := sh.Delete("held.fit"); err != nil {
		t.Fatalf("third Delete: %v", err)
	}

	f.FailNext(FakeOpRename, 1, SharingViolationLinkError("rename", "src.fit", "dst.fit"))
	if err := sh.Rename("src.fit", "dst.fit"); !IsInUse(err) {
		t.Fatalf("Rename err = %v, want IsInUse", err)
	}
	if err := sh.Rename("src.fit", "dst.fit"); err != nil {
		t.Fatalf("second Rename: %v", err)
	}

	// The writer fails partway through, then carries on; the failed Write adds
	// nothing.
	netErr := NetworkError("write")
	w, err := sh.CreateNew("out.fit")
	if err != nil {
		t.Fatal(err)
	}
	f.FailNext(FakeOpWrite, 1, netErr)
	if _, err := w.Write([]byte("ab")); !IsNetwork(err) {
		t.Fatalf("Write err = %v, want network", err)
	}
	if _, err := w.Write([]byte("cd")); err != nil {
		t.Fatalf("Write after the injected failure: %v", err)
	}
	w.Close()
	if got := readAll(t, sh, "out.fit"); got != "cd" {
		t.Fatalf("content = %q, want %q", got, "cd")
	}

	// Each op counts on its own; a session open can be failed too.
	f.FailNext(FakeOpOpen, 1, netErr)
	f.FailNext(FakeOpListDir, 1, netErr)
	if _, err := f.Open(t.Context()); !IsNetwork(err) {
		t.Fatalf("Open err = %v, want network", err)
	}
	if _, err := f.Open(t.Context()); err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if _, err := sh.ListDir(""); !IsNetwork(err) {
		t.Fatalf("ListDir err = %v, want network", err)
	}
	if _, err := sh.ListDir(""); err != nil {
		t.Fatalf("second ListDir: %v", err)
	}

	// n <= 0 disarms a pending injection.
	f.FailNext(FakeOpStat, 3, netErr)
	f.FailNext(FakeOpStat, 0, nil)
	if _, err := sh.Stat("dst.fit"); err != nil {
		t.Fatalf("Stat after disarming: %v", err)
	}
}

// Markers stay out of listings unless ListMarkers opts in; then they list as a
// device lists them, folders implied by a marker included.
func TestFakeListMarkersOptIn(t *testing.T) {
	f := NewFake()
	f.AddFile(wShare, "Autorun/a.fit", []byte("a"), time.Now())
	f.SetMarker(wShare, ".rig-id", []byte("root-marker"))
	f.SetMarker(wShare, "ASIAIR/.media-id", []byte("media"))
	sh := mustMount(t, mustOpen(t, f), wShare)

	if got := listNames(t, sh, ""); !equalStrings(got, []string{"Autorun/"}) {
		t.Fatalf("default root listing = %v, want markers hidden", got)
	}
	if _, err := sh.ListDir("ASIAIR"); !IsNotFound(err) {
		t.Fatalf("default ListDir(ASIAIR) err = %v, want not-found", err)
	}

	f.ListMarkers(true)
	if got, want := listNames(t, sh, ""), []string{".rig-id", "ASIAIR/", "Autorun/"}; !equalStrings(got, want) {
		t.Fatalf("root listing = %v, want %v", got, want)
	}
	entries, err := sh.ListDir("ASIAIR")
	if err != nil {
		t.Fatalf("ListDir(ASIAIR): %v", err)
	}
	if len(entries) != 1 || entries[0].Name != ".media-id" || entries[0].Size != int64(len("media")) || entries[0].IsDir {
		t.Fatalf("ListDir(ASIAIR) = %+v", entries)
	}

	f.ListMarkers(false)
	if got := listNames(t, sh, ""); !equalStrings(got, []string{"Autorun/"}) {
		t.Fatalf("listing after opting out = %v", got)
	}
}
