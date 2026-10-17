package logs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	testShare = "Udisk Images"
	testRoot  = "Autorun"
	testRig   = "rig-01"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mount opens a session on fake and mounts testShare, returning the Share the
// Puller consumes — mirroring how the rig worker hands its one per-session
// Share to Pull.
func mount(t *testing.T, fake *device.Fake) device.Share {
	t.Helper()
	ctx := context.Background()
	sess, err := fake.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	sh, err := sess.Mount(testShare)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return sh
}

func logPath(name string) string {
	return testRoot + "/log/" + name
}

func TestPullPHD2AndAutorunLogs(t *testing.T) {
	fake := device.NewFake()
	fake.AddShare(testShare)
	fake.AddFile(testShare, logPath("PHD2_GuideLog_2026-07-15_030000.txt"), []byte("phd2 guiding lines"), time.Now())
	fake.AddFile(testShare, logPath("Autorun_Log_2026-07-15_030000.txt"), []byte("autorun activity lines"), time.Now())

	fsRoot := t.TempDir()
	lfs := localfs.NewFake(fsRoot)
	st := newTestStore(t)
	p := New(lfs, st, fsRoot, nil)

	sh := mount(t, fake)
	ctx := context.Background()
	if err := p.Pull(ctx, sh, testRoot, testRig); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	assertFileContent(t, filepath.Join(fsRoot, testRig, "PHD2_GuideLog_2026-07-15_030000.txt"), "phd2 guiding lines")
	assertFileContent(t, filepath.Join(fsRoot, testRig, "Autorun_Log_2026-07-15_030000.txt"), "autorun activity lines")

	assertLogPull(t, st, ctx, logPath("PHD2_GuideLog_2026-07-15_030000.txt"), TypePHD2, int64(len("phd2 guiding lines")))
	assertLogPull(t, st, ctx, logPath("Autorun_Log_2026-07-15_030000.txt"), TypeAutorun, int64(len("autorun activity lines")))
}

func TestPullExcludesCHNTwin(t *testing.T) {
	fake := device.NewFake()
	fake.AddShare(testShare)
	fake.AddFile(testShare, logPath("Autorun_Log_2026-07-15_030000.txt"), []byte("english"), time.Now())
	fake.AddFile(testShare, logPath("Autorun_Log_2026-07-15_030000_CHN.txt"), []byte("chinese twin"), time.Now())

	fsRoot := t.TempDir()
	lfs := localfs.NewFake(fsRoot)
	st := newTestStore(t)
	p := New(lfs, st, fsRoot, nil)

	sh := mount(t, fake)
	ctx := context.Background()
	if err := p.Pull(ctx, sh, testRoot, testRig); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	assertFileContent(t, filepath.Join(fsRoot, testRig, "Autorun_Log_2026-07-15_030000.txt"), "english")
	if _, err := os.Stat(filepath.Join(fsRoot, testRig, "Autorun_Log_2026-07-15_030000_CHN.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected _CHN twin not to be pulled, stat err = %v", err)
	}

	pulls, err := st.ListLogPulls(ctx, testRig)
	if err != nil {
		t.Fatalf("ListLogPulls: %v", err)
	}
	if len(pulls) != 1 {
		t.Fatalf("expected exactly one recorded pull, got %d: %+v", len(pulls), pulls)
	}
}

func TestPullSkipsUnchangedSize(t *testing.T) {
	fake := device.NewFake()
	fake.AddShare(testShare)
	fake.AddFile(testShare, logPath("Autorun_Log_2026-07-15_030000.txt"), []byte("stable content"), time.Now())

	fsRoot := t.TempDir()
	lfs := localfs.NewFake(fsRoot)
	st := newTestStore(t)
	p := New(lfs, st, fsRoot, nil)
	ctx := context.Background()

	sh := mount(t, fake)
	if err := p.Pull(ctx, sh, testRoot, testRig); err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	first, ok, err := st.GetLogPull(ctx, testRig, logPath("Autorun_Log_2026-07-15_030000.txt"))
	if err != nil || !ok {
		t.Fatalf("GetLogPull after first pull: %v %v", ok, err)
	}

	// Overwrite the destination with a sentinel so a wrongly-repeated pull
	// would be detectable, then pull again with the device file unchanged.
	destPath := filepath.Join(fsRoot, testRig, "Autorun_Log_2026-07-15_030000.txt")
	if err := os.WriteFile(destPath, []byte("SENTINEL: must survive an unchanged-size pull"), 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	if err := p.Pull(ctx, sh, testRoot, testRig); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	second, ok, err := st.GetLogPull(ctx, testRig, logPath("Autorun_Log_2026-07-15_030000.txt"))
	if err != nil || !ok {
		t.Fatalf("GetLogPull after second pull: %v %v", ok, err)
	}
	if second.PulledAt != first.PulledAt {
		t.Fatalf("expected PulledAt unchanged on skip: first=%d second=%d", first.PulledAt, second.PulledAt)
	}
	assertFileContent(t, destPath, "SENTINEL: must survive an unchanged-size pull")
}

func TestPullReuploadsGrownLog(t *testing.T) {
	fake := device.NewFake()
	fake.AddShare(testShare)
	fake.AddFile(testShare, logPath("Autorun_Log_2026-07-15_030000.txt"), []byte("line one\n"), time.Now())

	fsRoot := t.TempDir()
	lfs := localfs.NewFake(fsRoot)
	st := newTestStore(t)
	p := New(lfs, st, fsRoot, nil)
	ctx := context.Background()

	sh := mount(t, fake)
	if err := p.Pull(ctx, sh, testRoot, testRig); err != nil {
		t.Fatalf("first Pull: %v", err)
	}

	grown := "line one\nline two\nline three\n"
	fake.AddFile(testShare, logPath("Autorun_Log_2026-07-15_030000.txt"), []byte(grown), time.Now())
	if err := p.Pull(ctx, sh, testRoot, testRig); err != nil {
		t.Fatalf("second Pull: %v", err)
	}

	destPath := filepath.Join(fsRoot, testRig, "Autorun_Log_2026-07-15_030000.txt")
	assertFileContent(t, destPath, grown)

	rec, ok, err := st.GetLogPull(ctx, testRig, logPath("Autorun_Log_2026-07-15_030000.txt"))
	if err != nil || !ok {
		t.Fatalf("GetLogPull: %v %v", ok, err)
	}
	if rec.Size != int64(len(grown)) {
		t.Fatalf("expected recorded size %d, got %d", len(grown), rec.Size)
	}
}

func TestPullMissingLogFolderTolerated(t *testing.T) {
	fake := device.NewFake()
	fake.AddShare(testShare)
	// No log/ folder created at all under root.

	fsRoot := t.TempDir()
	lfs := localfs.NewFake(fsRoot)
	st := newTestStore(t)
	p := New(lfs, st, fsRoot, nil)

	sh := mount(t, fake)
	if err := p.Pull(context.Background(), sh, testRoot, testRig); err != nil {
		t.Fatalf("expected missing log/ folder to be tolerated, got: %v", err)
	}

	// Also exercise the explicit not-found error path a real device might
	// return for a nonexistent directory (as opposed to the fake's natural
	// "no matching prefix" empty-list behavior).
	fake2 := device.NewFake()
	fake2.AddShare(testShare)
	fake2.FailListDir(testShare, device.NotFoundError("no such directory"))
	sh2 := mount(t, fake2)
	if err := p.Pull(context.Background(), sh2, testRoot, testRig); err != nil {
		t.Fatalf("expected NotFound ListDir error to be tolerated, got: %v", err)
	}
}

func TestPullIgnoresNonMatchingFiles(t *testing.T) {
	fake := device.NewFake()
	fake.AddShare(testShare)
	fake.AddFile(testShare, logPath("PHD2_GuideLog_2026-07-15_030000.txt"), []byte("guiding"), time.Now())
	fake.AddFile(testShare, logPath("PHD2_GuideLog_2026-07-15_030000_thn.jpg"), []byte("not a text file"), time.Now())
	fake.AddFile(testShare, logPath("random_notes.txt"), []byte("unrelated notes"), time.Now())
	fake.AddFile(testShare, logPath("stats.csv"), []byte("a,b,c"), time.Now())

	fsRoot := t.TempDir()
	lfs := localfs.NewFake(fsRoot)
	st := newTestStore(t)
	p := New(lfs, st, fsRoot, nil)
	ctx := context.Background()

	sh := mount(t, fake)
	if err := p.Pull(ctx, sh, testRoot, testRig); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	pulls, err := st.ListLogPulls(ctx, testRig)
	if err != nil {
		t.Fatalf("ListLogPulls: %v", err)
	}
	if len(pulls) != 1 || pulls[0].LogType != TypePHD2 {
		t.Fatalf("expected exactly the PHD2 log to be pulled, got %+v", pulls)
	}

	entries, err := os.ReadDir(filepath.Join(fsRoot, testRig))
	if err != nil {
		t.Fatalf("ReadDir dest: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "PHD2_GuideLog_2026-07-15_030000.txt" {
		t.Fatalf("expected exactly one dest file, got %v", entries)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name        string
		wantType    string
		wantMatched bool
	}{
		{"PHD2_GuideLog_2026-07-15_030000.txt", TypePHD2, true},
		{"phd2_guidelog_2026-07-15_030000.TXT", TypePHD2, true}, // case-insensitive
		{"Some_Guide_Log_thing.txt", TypePHD2, true},
		{"Autorun_Log_2026-07-15_030000.txt", TypeAutorun, true},
		{"AUTORUN_LOG_2026-07-15_030000.txt", TypeAutorun, true},
		{"Autorun_Log_2026-07-15_030000_CHN.txt", "", false},
		{"autorun_log_2026-07-15_030000_chn.txt", "", false}, // case-insensitive exclusion
		{"PHD2_GuideLog_2026-07-15_030000.jpg", "", false},   // wrong extension
		{"random_notes.txt", "", false},
		{"stats.csv", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotOK := classify(tc.name)
			if gotOK != tc.wantMatched || gotType != tc.wantType {
				t.Errorf("classify(%q) = (%q, %v), want (%q, %v)", tc.name, gotType, gotOK, tc.wantType, tc.wantMatched)
			}
		})
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("content of %s = %q, want %q", path, got, want)
	}
}

func assertLogPull(t *testing.T, st store.Store, ctx context.Context, sourcePath, wantType string, wantSize int64) {
	t.Helper()
	rec, ok, err := st.GetLogPull(ctx, testRig, sourcePath)
	if err != nil {
		t.Fatalf("GetLogPull(%s): %v", sourcePath, err)
	}
	if !ok {
		t.Fatalf("GetLogPull(%s): no row recorded", sourcePath)
	}
	if rec.LogType != wantType {
		t.Errorf("LogType = %q, want %q", rec.LogType, wantType)
	}
	if rec.Size != wantSize {
		t.Errorf("Size = %d, want %d", rec.Size, wantSize)
	}
	if rec.DestPath == "" {
		t.Errorf("DestPath is empty")
	}
	if rec.PulledAt == 0 {
		t.Errorf("PulledAt is zero")
	}
}
