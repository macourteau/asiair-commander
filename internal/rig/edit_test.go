package rig

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The header-edit invariants of design header-edits §11 (HE-1–HE-11 at the rig
// level), proved through the real worker on synthetic fixtures built with
// internal/header/fitstest: a mono frame written without a filter wheel, which
// carries no FILTER card, in the two layouts §4.1 distinguishes.
const (
	// freeSlotCards leaves the END block with free slots, so inserting FILTER keeps
	// the file's length.
	freeSlotCards = 17
	// fullBlockCards puts END in the 36th slot of its block, so inserting FILTER
	// grows the header by one 2880-byte block.
	fullBlockCards = 35

	editFlatSrc  = "Autorun/Flat/Flat_0001.fit"
	editLightSrc = "Autorun/Light/C-2025 A6/Light_0001.fit"
)

// filterlessCards returns n header cards (END not counted) for a synthetic frame of
// the given IMAGETYP that carries no FILTER card: fitstest's light header without
// it, padded with commentary cards.
func filterlessCards(t *testing.T, imagetyp string, n int) []string {
	t.Helper()
	var out []string
	for _, c := range fitstest.LightCards(revW, revH, revTarget) {
		switch {
		case strings.HasPrefix(c, "FILTER  ="):
			continue
		case strings.HasPrefix(c, "IMAGETYP="):
			c = fmt.Sprintf("IMAGETYP= %-20s / Type of image", "'"+imagetyp+"'")
		}
		out = append(out, c)
	}
	if len(out) > n {
		t.Fatalf("fixture: %d base cards do not fit in %d", len(out), n)
	}
	for i := 0; len(out) < n; i++ {
		out = append(out, fmt.Sprintf("COMMENT   synthetic padding card %d", i))
	}
	return out
}

// filterlessFrame builds a filterless frame over a real synthetic data unit.
func filterlessFrame(t *testing.T, imagetyp string, n int) []byte {
	t.Helper()
	return fitstest.Build(filterlessCards(t, imagetyp, n), fitstest.SyntheticImage(revW, revH))
}

// fileFilterless files a filterless frame of the given type and layout and returns
// its verified row.
func fileFilterless(t *testing.T, h *harness, src, imagetyp string, n int) (*model.Frame, []byte) {
	t.Helper()
	body := filterlessFrame(t, imagetyp, n)
	f := pullFile(t, h, src, body, 10*time.Minute)
	if f.State != model.StateVerified || f.Filter != "" || f.DestPath == "" {
		t.Fatalf("setup: state=%s filter=%q dest=%q", f.State, f.Filter, f.DestPath)
	}
	return f, body
}

// editOf runs one edit through the worker's request service, as the API does.
func editOf(t *testing.T, h *harness, id int64, key, value string) model.EditResult {
	t.Helper()
	res, err := h.w.serviceEdit(context.Background(), id, key, value)
	if err != nil {
		t.Fatalf("serviceEdit(%d, %s=%q): %v", id, key, value, err)
	}
	return res
}

// mustEdit runs one edit that must land and returns the row it left.
func mustEdit(t *testing.T, h *harness, id int64, key, value string) model.Frame {
	t.Helper()
	if res := editOf(t, h, id, key, value); res.Status != editEdited || res.Reason != "" {
		t.Fatalf("edit %d %s=%q: status=%q reason=%q", id, key, value, res.Status, res.Reason)
	}
	return mustGetFrame(t, h, id)
}

// filterOf reads the FILTER card a file actually carries.
func filterOf(t *testing.T, h *harness, path string) (string, bool) {
	t.Helper()
	hdr, err := h.w.hdr.ReadHeader(path)
	if err != nil {
		t.Fatalf("read header %s: %v", path, err)
	}
	return hdr.Str("FILTER")
}

// fileSize stats a file the test expects to be there.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Size()
}

// filterInFolderTemplate makes the flat template name <FILTER>, so a filter edit
// changes the folder and takes the filing path.
func filterInFolderTemplate(h *harness) {
	h.cfg.Templates.Cal[model.TypeFlat] = "<SENSOR>/FLAT/<FILTER>/<OTA>/<GAIN>/<BINNING>/<DATE>"
}

// wantArchiveConsistent asserts the archive-side anchors describe the file at
// dest_path exactly: its whole-file hash at its own length (HE-6), and the
// exposure identity the row records.
func wantArchiveConsistent(t *testing.T, h *harness, f model.Frame) {
	t.Helper()
	if n := fileSize(t, f.DestPath); n != f.ArchiveSize {
		t.Fatalf("archive_size=%d, the file is %d bytes", f.ArchiveSize, n)
	}
	if got := hashOnDisk(t, f.DestPath); got != f.ArchiveSHA256 {
		t.Fatalf("archive_sha256=%s, the file hashes to %s", f.ArchiveSHA256, got)
	}
	if f.DataSHA256 != "" {
		if got := dataDigestOf(readLocal(t, f.DestPath)); got != f.DataSHA256 {
			t.Fatalf("the file's data unit %s is not the row's exposure %s", got, f.DataSHA256)
		}
	}
}

// TestEdit_InPlaceKeepsPath (HE-10): a FILTER edit under the shipped flat template,
// which does not name <FILTER>, supersedes the archive in place: the frame keeps its
// path and its filename, no rot_clash is raised, and the columns an edit has no
// reason to touch — the device anchor, state, pulled_at, delete_defer_count — are
// unchanged. The insertion fits a free slot, so the length is unchanged too
// (design header-edits AC1, AC2).
func TestEdit_InPlaceKeepsPath(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, body := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	if err := h.store.DeferDelete(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	before := mustGetFrame(t, h, f.ID)
	if before.DeleteDeferCount != 1 {
		t.Fatalf("setup: delete_defer_count=%d", before.DeleteDeferCount)
	}
	swapped := false
	h.w.crashAfterSupersedeStep = func(int, string) bool { swapped = true; return false }

	res := editOf(t, h, f.ID, "FILTER", "L")
	if res.Status != editEdited || res.Reason != "" {
		t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
	}
	if !swapped {
		t.Fatal("a FILTER edit whose route is unchanged must supersede in place")
	}
	got := mustGetFrame(t, h, f.ID)
	if got.DestPath != before.DestPath || res.NewPath != before.DestPath || res.OldPath != before.DestPath {
		t.Fatalf("path moved: row %q result %q -> %q, want %q", got.DestPath, res.OldPath, res.NewPath, before.DestPath)
	}
	if ents := dirEntries(t, filepath.Dir(got.DestPath)); len(ents) != 1 || ents[0] != filepath.Base(before.DestPath) {
		t.Fatalf("the folder must hold exactly the frame under its own name, got %v", ents)
	}
	if h.hasAlert(model.AlertRotClash) {
		t.Fatalf("an in-place edit must not raise rot_clash: %v", h.raisedCategories())
	}
	if got.SHA256 != before.SHA256 || got.Size != before.Size || got.Mtime != before.Mtime ||
		got.DataSHA256 != before.DataSHA256 {
		t.Fatalf("device anchor moved: sha=%q size=%d mtime=%d data=%q", got.SHA256, got.Size, got.Mtime, got.DataSHA256)
	}
	if got.State != before.State || got.PulledAt != before.PulledAt || got.DeleteDeferCount != before.DeleteDeferCount {
		t.Fatalf("lifecycle moved: state=%s pulled_at=%d defer=%d", got.State, got.PulledAt, got.DeleteDeferCount)
	}
	if got.Filter != "L" || got.EditSHA256 == "" || got.EditSHA256 != got.ArchiveSHA256 {
		t.Fatalf("overlay: filter=%q edit=%q archive=%q", got.Filter, got.EditSHA256, got.ArchiveSHA256)
	}
	if e, ok := got.HeaderEdits.Lookup("FILTER"); !ok || e.Value != "L" || e.From != nil || len(got.HeaderEdits) != 1 {
		t.Fatalf("header_edits=%+v, want one FILTER entry with no original", got.HeaderEdits)
	}
	if got.ArchiveSize != int64(len(body)) {
		t.Fatalf("an insertion into a free slot keeps the length: %d != %d", got.ArchiveSize, len(body))
	}
	wantArchiveConsistent(t, h, got)
	if v, ok := filterOf(t, h, got.DestPath); !ok || v != "L" {
		t.Fatalf("the archive reads FILTER=%q (present %v), want L", v, ok)
	}
	if len(openSupersedes(t, h)) != 0 {
		t.Fatal("a completed in-place edit leaves no intent open")
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatal("edit temp leaked")
	}
}

// TestEdit_FilterTemplateRefiles: under a flat template naming <FILTER> the edited
// flat is re-filed into the filter's folder through the filing path (design
// header-edits AC3).
func TestEdit_FilterTemplateRefiles(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	filterInFolderTemplate(h)
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	swapped := false
	h.w.crashAfterSupersedeStep = func(int, string) bool { swapped = true; return false }

	got := mustEdit(t, h, f.ID, "FILTER", "L")
	if swapped {
		t.Fatal("a FILTER edit that changes the folder must take the filing path")
	}
	if !strings.Contains(got.DestPath, string(filepath.Separator)+"L"+string(filepath.Separator)) || got.DestPath == f.DestPath {
		t.Fatalf("dest=%q, want a path under an L folder", got.DestPath)
	}
	if fileExists(f.DestPath) {
		t.Fatalf("the old archive must be removed once the commit is proven: %s", f.DestPath)
	}
	wantArchiveConsistent(t, h, got)
}

// TestEdit_PreservesDeviceAnchor (HE-1): neither commit mutates sha256 or size.
func TestEdit_PreservesDeviceAnchor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refile bool
	}{{"in place", false}, {"filing path", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			if tc.refile {
				filterInFolderTemplate(h)
			}
			f, body := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
			got := mustEdit(t, h, f.ID, "FILTER", "Ha")
			if got.SHA256 != f.SHA256 || got.SHA256 != sha256hex(body) || got.Size != int64(len(body)) || got.Mtime != f.Mtime {
				t.Fatalf("device anchor moved: sha=%q size=%d mtime=%d", got.SHA256, got.Size, got.Mtime)
			}
			if got.ArchiveSHA256 == got.SHA256 {
				t.Fatal("setup: the archive must now differ from the device copy")
			}
		})
	}
}

// TestEdit_DeviceOriginalUntouched (HE-1): an edit issues no device-mutating call
// and leaves the device original byte-identical.
func TestEdit_DeviceOriginalUntouched(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, body := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
	before := h.dev.Mutations()

	got := mustEdit(t, h, f.ID, "FILTER", "L")
	if n := h.dev.Mutations(); n != before {
		t.Fatalf("the edit issued %d device-mutating call(s)", n-before)
	}
	if !h.deviceHas(editFlatSrc) || deviceFileHash(t, h, editFlatSrc) != sha256hex(body) {
		t.Fatal("the device original must survive byte-identical")
	}
	if got.EditSHA256 == "" {
		t.Fatal("setup: the edit did not happen, which makes this test vacuous")
	}
}

// TestEdit_RefusesDriftedArchive (HE-2): an archive that no longer hashes to
// archiveHash(f) at archive_size is never edited.
func TestEdit_RefusesDriftedArchive(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	drifted := flipLastByte(t, readLocal(t, f.DestPath))
	writeLocal(t, f.DestPath, drifted)

	res := editOf(t, h, f.ID, "FILTER", "L")
	if res.Status != editFailed || !strings.Contains(res.Reason, "drifted") {
		t.Fatalf("status=%q reason=%q, want the drift refusal", res.Status, res.Reason)
	}
	got := mustGetFrame(t, h, f.ID)
	if got.EditSHA256 != "" || got.ArchiveSHA256 != f.ArchiveSHA256 || got.Filter != "" || len(got.HeaderEdits) != 0 {
		t.Fatalf("a refused edit changed the row: %+v", got)
	}
	if !bytes.Equal(readLocal(t, f.DestPath), drifted) {
		t.Fatal("a refused edit must not touch the archive")
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatal("edit temp leaked")
	}
}

// headerBlocksOf splits a FITS file into its header card images (through the end of
// the block holding END), the index of END among them, and the data unit.
func headerBlocksOf(t *testing.T, b []byte) ([][]byte, int, []byte) {
	t.Helper()
	var cards [][]byte
	for off := 0; off+80 <= len(b); off += 80 {
		cards = append(cards, append([]byte(nil), b[off:off+80]...))
		if strings.TrimRight(string(b[off:off+8]), " ") == "END" {
			end := len(cards) - 1
			for len(cards)%36 != 0 {
				off += 80
				cards = append(cards, append([]byte(nil), b[off:off+80]...))
			}
			return cards, end, b[len(cards)*80:]
		}
	}
	t.Fatal("no END card")
	return nil, 0, nil
}

// joinFITS reassembles header cards and a data unit.
func joinFITS(cards [][]byte, data []byte) []byte {
	var out []byte
	for _, c := range cards {
		out = append(out, c...)
	}
	return append(out, data...)
}

// cardIndex finds the card whose keyword is key.
func cardIndex(t *testing.T, cards [][]byte, key string) int {
	t.Helper()
	for i, c := range cards {
		if strings.TrimRight(string(c[:8]), " ") == key {
			return i
		}
	}
	t.Fatalf("no %s card", key)
	return -1
}

// TestEdit_RefusesCorruptingWriters (HE-3): a writer that drops, reorders or
// alters a card or a card's comment, alters a data byte, mis-reports its growth or
// leaves non-blank padding is refused by the worker's own reads before anything is
// committed — with the writer's own proof digests agreeing every time, so none of
// these is caught by the writer's self-report.
func TestEdit_RefusesCorruptingWriters(t *testing.T) {
	blank := []byte(strings.Repeat(" ", 80))
	for _, tc := range []struct {
		name string
		// withFilter files a frame that already carries FILTER, so the edit replaces
		// rather than inserts.
		withFilter bool
		cards      int
		mutate     func(t *testing.T, b []byte) []byte
		forge      func(*header.EditResult)
		// caught names the worker check that must refuse it.
		caught string
	}{
		{name: "drops a card", caught: "cards through END", cards: freeSlotCards, mutate: func(t *testing.T, b []byte) []byte {
			cards, _, data := headerBlocksOf(t, b)
			i := cardIndex(t, cards, "TELESCOP")
			cards = append(append(cards[:i:i], cards[i+1:]...), blank)
			return joinFITS(cards, data)
		}},
		{name: "reorders two cards", caught: "edited header card", cards: freeSlotCards, mutate: func(t *testing.T, b []byte) []byte {
			cards, _, data := headerBlocksOf(t, b)
			i, j := cardIndex(t, cards, "GAIN"), cardIndex(t, cards, "CCD-TEMP")
			cards[i], cards[j] = cards[j], cards[i]
			return joinFITS(cards, data)
		}},
		{name: "alters a card's comment", caught: "edited header card", cards: freeSlotCards, mutate: func(t *testing.T, b []byte) []byte {
			cards, _, data := headerBlocksOf(t, b)
			i := cardIndex(t, cards, "GAIN")
			copy(cards[i], bytes.Replace(cards[i], []byte("Sensor gain"), []byte("Sensor GAIN"), 1))
			return joinFITS(cards, data)
		}},
		{name: "alters a card's value", caught: "edited header card", cards: freeSlotCards, mutate: func(t *testing.T, b []byte) []byte {
			cards, _, data := headerBlocksOf(t, b)
			i := cardIndex(t, cards, "EXPTIME")
			copy(cards[i], bytes.Replace(cards[i], []byte("300.0"), []byte("30.00"), 1))
			return joinFITS(cards, data)
		}},
		{name: "adds a comment to the edited card", caught: "edited header card", cards: freeSlotCards, mutate: func(t *testing.T, b []byte) []byte {
			cards, _, data := headerBlocksOf(t, b)
			i := cardIndex(t, cards, "FILTER")
			copy(cards[i][30:], "/ edited")
			return joinFITS(cards, data)
		}},
		{name: "corrupts a data byte", caught: "data unit differs", cards: freeSlotCards, mutate: flipLastByte},
		{name: "grows the header by a block it was not asked to", caught: "cards through END", cards: freeSlotCards, mutate: func(t *testing.T, b []byte) []byte {
			cards, end, data := headerBlocksOf(t, b)
			grown := append([][]byte(nil), cards[:end]...)
			for i := 0; i < 36; i++ {
				grown = append(grown, blank)
			}
			grown = append(grown, cards[end:]...)
			return joinFITS(grown, data)
		}},
		{name: "reports a growth the file does not have", caught: "the writer reported", cards: fullBlockCards,
			mutate: func(_ *testing.T, b []byte) []byte { return b },
			forge:  func(er *header.EditResult) { er.OutputSize -= 2880 }},
		{name: "pads the grown block with non-blanks", caught: "non-blank padding", cards: fullBlockCards, mutate: func(t *testing.T, b []byte) []byte {
			cards, end, data := headerBlocksOf(t, b)
			copy(cards[end+1], "X")
			return joinFITS(cards, data)
		}},
		{name: "changes the padding of a replace-only edit", caught: "changed the padding", withFilter: true, cards: freeSlotCards + 1, mutate: func(t *testing.T, b []byte) []byte {
			cards, end, data := headerBlocksOf(t, b)
			copy(cards[end+1], "X")
			return joinFITS(cards, data)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			var f *model.Frame
			if tc.withFilter {
				cards := append(filterlessCards(t, "Flat", tc.cards-1), "FILTER  = 'HA      '           / Filter used")
				f = pullFile(t, h, editFlatSrc, fitstest.Build(cards, fitstest.SyntheticImage(revW, revH)), 10*time.Minute)
				if f.Filter != "HA" {
					t.Fatalf("setup: filter=%q", f.Filter)
				}
			} else {
				f, _ = fileFilterless(t, h, editFlatSrc, "Flat", tc.cards)
			}
			archive := readLocal(t, f.DestPath)
			h.w.rewriter = corruptingWriter(t, tc.mutate, tc.forge)

			res := editOf(t, h, f.ID, "FILTER", "L")
			if res.Status != editFailed || !strings.HasPrefix(res.Reason, "edit:") || !strings.Contains(res.Reason, tc.caught) {
				t.Fatalf("status=%q reason=%q, want the worker's own %q check to refuse", res.Status, res.Reason, tc.caught)
			}
			got := mustGetFrame(t, h, f.ID)
			if got.EditSHA256 != "" || got.ArchiveSHA256 != f.ArchiveSHA256 || got.ArchiveSize != f.ArchiveSize ||
				got.Filter != f.Filter || len(got.HeaderEdits) != 0 || got.DestPath != f.DestPath {
				t.Fatalf("a refused edit changed the row: %+v", got)
			}
			if !bytes.Equal(readLocal(t, f.DestPath), archive) {
				t.Fatal("a refused edit must leave the archive byte-identical")
			}
			if len(openSupersedes(t, h)) != 0 || !stagingEmpty(t, h.dir) {
				t.Fatal("a refused edit leaves no intent and no temp")
			}
		})
	}
}

// TestEdit_InsertGrowsFullBlock (HE-4, through the worker): inserting FILTER into a
// header whose END sits in the 36th slot grows the archive by exactly 2880 bytes and
// leaves its data unit — data_sha256 — unchanged.
func TestEdit_InsertGrowsFullBlock(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, body := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
	if fitstest.HeaderBlocks(filterlessCards(t, "Flat", fullBlockCards)) != 1 {
		t.Fatal("setup: the fixture's header must be one full block")
	}
	got := mustEdit(t, h, f.ID, "FILTER", "L")
	if got.ArchiveSize != int64(len(body))+2880 {
		t.Fatalf("archive_size=%d, want %d", got.ArchiveSize, len(body)+2880)
	}
	if got.DataSHA256 == "" || got.DataSHA256 != f.DataSHA256 || dataDigestOf(readLocal(t, got.DestPath)) != dataDigestOf(body) {
		t.Fatal("the data unit must survive the growth unchanged")
	}
	if got.DestPath != f.DestPath {
		t.Fatalf("a grown in-place edit keeps its path: %q", got.DestPath)
	}
}

// TestEdit_GrownArchiveSizeRecorded (HE-6): after an edit that grows the file,
// archive_size is the file's own length on either commit, so no later archive-side
// comparison reads a prefix.
func TestEdit_GrownArchiveSizeRecorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refile bool
	}{{"in place", false}, {"filing path", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			if tc.refile {
				filterInFolderTemplate(h)
			}
			f, body := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
			got := mustEdit(t, h, f.ID, "FILTER", "L")
			if got.ArchiveSize != int64(len(body))+2880 {
				t.Fatalf("archive_size=%d, want the grown length %d", got.ArchiveSize, len(body)+2880)
			}
			if tc.refile == (got.DestPath == f.DestPath) {
				t.Fatalf("dest=%q: the commit taken does not match the template", got.DestPath)
			}
			wantArchiveConsistent(t, h, got)
		})
	}
}

// TestReapplyEdits_GrownSizeOnDiffer (HE-6): a device-mtime jitter of identical
// bytes re-pulls an edited, grown frame; the re-apply reproduces the grown archive
// and the filing ladder recognises it at the GROWN length, as an idempotent keep —
// not, at the device length, as a content clash filed under a token with a
// rot_clash alert.
func TestReapplyEdits_GrownSizeOnDiffer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, body := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
	edited := mustEdit(t, h, f.ID, "FILTER", "L")

	h.nowVal = h.nowVal.Add(time.Minute)
	h.dev.AddFile(emmc, editFlatSrc, body, h.nowVal.Add(-5*time.Minute))
	h.cycle()

	got := mustGetFrame(t, h, f.ID)
	if h.hasAlert(model.AlertRotClash) {
		t.Fatalf("the re-pull must adopt the archive, not clash with it: %v", h.raisedCategories())
	}
	if ents := dirEntries(t, filepath.Dir(edited.DestPath)); len(ents) != 1 {
		t.Fatalf("one file, not two: %v", ents)
	}
	if got.DestPath != edited.DestPath || got.ArchiveSHA256 != edited.ArchiveSHA256 || got.ArchiveSize != edited.ArchiveSize {
		t.Fatalf("re-pull moved the archive anchors: dest=%q sha=%q size=%d", got.DestPath, got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.Filter != "L" || got.EditSHA256 != edited.EditSHA256 {
		t.Fatalf("overlay lost on the re-pull: filter=%q edit=%q", got.Filter, got.EditSHA256)
	}
	wantArchiveConsistent(t, h, got)
}

// TestRestore_ReappliesGrownEdit (HE-6): a restore of an edited, grown frame
// re-applies the edit to the device stream, recognises the result at the GROWN
// length as the archive expectation, and lands it with archive_size intact.
func TestRestore_ReappliesGrownEdit(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
	edited := mustEdit(t, h, f.ID, "FILTER", "L")

	if err := os.Remove(edited.DestPath); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, f.ID, 0)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}
	h.cycle()

	got := mustGetFrame(t, h, f.ID)
	if got.RestoreRequestedAt != 0 || got.MissingSeenAt != 0 {
		t.Fatalf("the restore did not complete: missing=%d restore=%d", got.MissingSeenAt, got.RestoreRequestedAt)
	}
	if got.ArchiveSHA256 != edited.ArchiveSHA256 || got.ArchiveSize != edited.ArchiveSize || got.EditSHA256 != edited.EditSHA256 {
		t.Fatalf("restore changed the archive expectation: sha=%q size=%d edit=%q", got.ArchiveSHA256, got.ArchiveSize, got.EditSHA256)
	}
	wantArchiveConsistent(t, h, got)
	if v, _ := filterOf(t, h, got.DestPath); v != "L" {
		t.Fatalf("the restored archive reads FILTER=%q, want L", v)
	}
}

// TestSupersede_GrownEditSize (HE-6): a device revision of an edited frame whose
// revised header has no free slot left — END in the last slot after the device
// injects its WCS in place — re-applies the FILTER insertion, which grows the
// re-applied file past the device length; the supersede records that length.
func TestSupersede_GrownEditSize(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	cards := filterlessCards(t, "Light", fullBlockCards-12)
	original, revised := fitstest.SameSizeRevision(cards, fitstest.SyntheticImage(revW, revH))
	f := pullFile(t, h, editLightSrc, original, 10*time.Minute)
	edited := mustEdit(t, h, f.ID, "FILTER", "L")
	if edited.ArchiveSize != int64(len(original)) {
		t.Fatalf("setup: the first edit fits a free slot (%d != %d)", edited.ArchiveSize, len(original))
	}

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	h.dev.AddFile(emmc, editLightSrc, revised, h.nowVal.Add(-5*time.Minute))
	h.cycle()

	got := mustGetFrame(t, h, f.ID)
	if got.SHA256 != sha256hex(revised) || got.Size != int64(len(revised)) {
		t.Fatalf("the device anchor must describe the revised copy: sha=%q size=%d", got.SHA256, got.Size)
	}
	if got.ArchiveSize != int64(len(revised))+2880 {
		t.Fatalf("archive_size=%d, want the re-applied, grown length %d", got.ArchiveSize, len(revised)+2880)
	}
	if got.DestPath != edited.DestPath || got.Filter != "L" || got.EditSHA256 != got.ArchiveSHA256 {
		t.Fatalf("supersede: dest=%q filter=%q edit=%q archive=%q", got.DestPath, got.Filter, got.EditSHA256, got.ArchiveSHA256)
	}
	wantArchiveConsistent(t, h, got)
}

// TestDelete_EditedGrownThreeWay (HE-7): the delete pass's three-way check passes
// for an edited, grown archive and releases the device original — and only when
// the device copy hashes to sha256 and the archive to its anchor at its length.
func TestDelete_EditedGrownThreeWay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, h *harness, f model.Frame, body []byte)
		deleted bool
	}{
		{name: "all three agree", deleted: true},
		{name: "device copy differs", corrupt: func(t *testing.T, h *harness, f model.Frame, body []byte) {
			// Same length and mtime, so the pre-delete gate passes and the hash decides.
			h.dev.AddFile(emmc, editFlatSrc, flipLastByte(t, append([]byte(nil), body...)), time.Unix(f.Mtime, 0))
		}},
		{name: "archive differs", corrupt: func(t *testing.T, h *harness, f model.Frame, _ []byte) {
			writeLocal(t, f.DestPath, flipLastByte(t, readLocal(t, f.DestPath)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			confirmDeleteIdentity(h)
			f, body := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
			edited := mustEdit(t, h, f.ID, "FILTER", "L")
			if edited.ArchiveSize == edited.Size {
				t.Fatal("setup: the archive must have grown past the device length")
			}
			if tc.corrupt != nil {
				tc.corrupt(t, h, edited, body)
			}

			h.nowVal = h.nowVal.Add(2 * time.Hour)
			h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

			got := mustGetFrame(t, h, f.ID)
			if tc.deleted {
				if h.deviceHas(editFlatSrc) || got.State != model.StateDeleted {
					t.Fatalf("the device original must be released: on device=%v state=%s", h.deviceHas(editFlatSrc), got.State)
				}
				if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
					t.Fatalf("a healthy edited archive raised %v", h.raisedCategories())
				}
				wantArchiveConsistent(t, h, got)
				return
			}
			if !h.deviceHas(editFlatSrc) || got.State == model.StateDeleted {
				t.Fatalf("a failed three-way check must keep the device original: state=%s", got.State)
			}
		})
	}
}

// TestReapplyEdits_ReproducesMultiStepArchive (HE-8, design header-edits AC5): an
// overlay built in several steps — FILTER inserted, OBJECT retargeted, FILTER
// changed — applied in one pass to the byte-identical device copy reproduces the
// archive byte for byte, and a re-pull after a device-mtime change keeps both edits
// and the archive hash.
func TestReapplyEdits_ReproducesMultiStepArchive(t *testing.T) {
	for _, n := range []int{freeSlotCards, fullBlockCards} {
		t.Run(fmt.Sprintf("%d cards", n), func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			f, body := fileFilterless(t, h, editLightSrc, "Light", n)
			mustEdit(t, h, f.ID, "FILTER", "L")
			mustEdit(t, h, f.ID, "OBJECT", "M31")
			row := mustEdit(t, h, f.ID, "FILTER", "Ha")
			if len(row.HeaderEdits) != 2 || row.HeaderEdits[0].Key != "FILTER" || row.HeaderEdits[1].Key != "OBJECT" {
				t.Fatalf("header_edits=%+v, want FILTER then OBJECT in first-edit order", row.HeaderEdits)
			}
			if row.HeaderEdits[0].Value != "Ha" || row.HeaderEdits[0].From != nil ||
				row.HeaderEdits[1].From == nil || *row.HeaderEdits[1].From != revTarget {
				t.Fatalf("header_edits=%+v", row.HeaderEdits)
			}
			archive := readLocal(t, row.DestPath)

			tmp := filepath.Join(h.cfg.Roots.Staging, "device-copy.tmp")
			writeLocal(t, tmp, body)
			out, sum, size, err := h.w.reapplyEditsIfNeeded(h.cfg, tmp, sha256hex(body), dataDigestOf(body), int64(len(body)), &row)
			if err != nil || out == tmp {
				t.Fatalf("re-apply: out=%q err=%v", out, err)
			}
			if !bytes.Equal(readLocal(t, out), archive) || sum != row.ArchiveSHA256 || size != row.ArchiveSize {
				t.Fatalf("a one-pass re-apply must reproduce the archive: hash %s vs %s, size %d vs %d", sum, row.ArchiveSHA256, size, row.ArchiveSize)
			}
			_ = os.Remove(out)
			_ = os.Remove(tmp)

			h.nowVal = h.nowVal.Add(time.Minute)
			h.dev.AddFile(emmc, editLightSrc, body, h.nowVal.Add(-5*time.Minute))
			h.cycle()
			got := mustGetFrame(t, h, f.ID)
			if got.ArchiveSHA256 != row.ArchiveSHA256 || got.DestPath != row.DestPath || got.Target != "M31" || got.Filter != "Ha" {
				t.Fatalf("re-pull: sha=%q dest=%q target=%q filter=%q", got.ArchiveSHA256, got.DestPath, got.Target, got.Filter)
			}
			if h.hasAlert(model.AlertRotClash) {
				t.Fatalf("the re-pull must adopt the archive: %v", h.raisedCategories())
			}
		})
	}
}

// TestEditedFrameSurvivesARevision (HE-9): a device-side revision of a
// FILTER-edited frame — the ASIAIR injecting its WCS, growing the header — is the
// same exposure, so the edit is re-applied on top of the revised header and the
// archive superseded in place with the overlay kept.
func TestEditedFrameSurvivesARevision(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	original, revised := fitstest.Revision(filterlessCards(t, "Light", freeSlotCards), fitstest.SyntheticImage(revW, revH))
	f := pullFile(t, h, editLightSrc, original, 10*time.Minute)
	edited := mustEdit(t, h, f.ID, "FILTER", "L")

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	h.dev.AddFile(emmc, editLightSrc, revised, h.nowVal.Add(-5*time.Minute))
	h.cycle()

	got := mustGetFrame(t, h, f.ID)
	if got.SHA256 != sha256hex(revised) || got.Size != int64(len(revised)) {
		t.Fatalf("the device anchor must describe the revised copy: sha=%q size=%d", got.SHA256, got.Size)
	}
	if got.DestPath != edited.DestPath || len(dirEntries(t, filepath.Dir(got.DestPath))) != 1 {
		t.Fatalf("the revision must supersede in place: %q %v", got.DestPath, dirEntries(t, filepath.Dir(got.DestPath)))
	}
	if got.Filter != "L" || len(got.HeaderEdits) != 1 || got.EditSHA256 != got.ArchiveSHA256 || got.ArchiveSHA256 == edited.ArchiveSHA256 {
		t.Fatalf("overlay: filter=%q edits=%+v edit=%q archive=%q", got.Filter, got.HeaderEdits, got.EditSHA256, got.ArchiveSHA256)
	}
	wantArchiveConsistent(t, h, got)
	hdr, err := h.w.hdr.ReadHeader(got.DestPath)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := hdr.Str("FILTER"); v != "L" {
		t.Fatalf("FILTER=%q after the revision, want L", v)
	}
	if _, ok := hdr.Str("CTYPE1"); !ok {
		t.Fatal("the archive must carry the device's revised header")
	}
}

// TestEdit_InPlaceCommitsSharers (HE-10): the in-place commit advances every row
// sharing the archive file to the edited anchors and overlay, and leaves each
// sharer's own device anchor and lifecycle alone.
func TestEdit_InPlaceCommitsSharers(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, body := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	h.dev.AddShare(tf)
	h.profile.Sources = append(h.profile.Sources, model.Source{Share: tf, Roots: []string{"Autorun"}})
	h.dev.AddFile(tf, editFlatSrc, body, h.nowVal.Add(-10*time.Minute))
	h.cycle()
	rows, err := h.store.FramesByDestPath(ctx, "rig-01", f.DestPath)
	if err != nil || len(rows) != 2 {
		t.Fatalf("setup: rows=%d err=%v", len(rows), err)
	}
	var sharer model.Frame
	for _, r := range rows {
		if r.ID != f.ID {
			sharer = r
		}
	}

	edited := mustEdit(t, h, f.ID, "FILTER", "L")
	got := mustGetFrame(t, h, sharer.ID)
	if got.ArchiveSHA256 != edited.ArchiveSHA256 || got.ArchiveSize != edited.ArchiveSize ||
		got.EditSHA256 != edited.EditSHA256 || got.Filter != "L" || len(got.HeaderEdits) != 1 {
		t.Fatalf("the sharer must describe the edited file: %+v", got)
	}
	if got.SHA256 != sharer.SHA256 || got.Size != sharer.Size || got.Mtime != sharer.Mtime ||
		got.State != sharer.State || got.PulledAt != sharer.PulledAt || got.DestPath != sharer.DestPath {
		t.Fatalf("the sharer's own device anchor and lifecycle are not the edit's: %+v", got)
	}
	wantArchiveConsistent(t, h, got)
}

// TestEdit_RefusesOpenIntent (HE-10): an open supersede or re-file intent at the
// archive's path refuses the edit as skipped, before anything moves — a second swap
// would overwrite the first's intent and orphan its copy set aside.
func TestEdit_RefusesOpenIntent(t *testing.T) {
	for _, tc := range []struct {
		name string
		put  func(t *testing.T, h *harness, f *model.Frame)
	}{
		{"supersede intent", func(t *testing.T, h *harness, f *model.Frame) {
			if err := h.store.PutSupersedeIntent(context.Background(), store.SupersedeIntent{
				RigID: "rig-01", DestPath: f.DestPath, AsidePath: f.DestPath + supersedeAsideSuffix + "0123456789abcdef",
				FrameID: f.ID, SHA256: f.SHA256, Size: f.Size, ArchiveSHA256: f.ArchiveSHA256, ArchiveSize: f.ArchiveSize,
				State: f.State, PulledAt: f.PulledAt, CreatedAt: h.nowVal.Unix(),
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"re-file intent", func(t *testing.T, h *harness, f *model.Frame) {
			if err := h.store.PutRefileIntent(context.Background(), store.RefileIntent{
				RigID: "rig-01", FromPath: f.DestPath, ToPath: f.DestPath + ".elsewhere", CreatedAt: h.nowVal.Unix(),
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
			archive := readLocal(t, f.DestPath)
			tc.put(t, h, f)

			res := editOf(t, h, f.ID, "FILTER", "L")
			if res.Status != editSkipped || !strings.Contains(res.Reason, "has not finished") {
				t.Fatalf("status=%q reason=%q, want skipped over the open intent", res.Status, res.Reason)
			}
			if got := mustGetFrame(t, h, f.ID); got.EditSHA256 != "" || got.Filter != "" {
				t.Fatalf("a refused edit changed the row: %+v", got)
			}
			if !bytes.Equal(readLocal(t, f.DestPath), archive) {
				t.Fatal("a refused edit must not touch the archive")
			}
		})
	}
}

// TestEdit_InPlacePrunedRowFails (HE-10): the hourly prune can remove a deleted
// row mid-swap; the swap then leaves the edited file in place and returns no
// error, and the edit reports failed because no row carries the edited hash.
func TestEdit_InPlacePrunedRowFails(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	del := *f
	del.State, del.DeletedAt = model.StateDeleted, 1
	if err := h.store.UpdateFrame(ctx, &del); err != nil {
		t.Fatal(err)
	}
	h.w.crashAfterSupersedeStep = func(step int, _ string) bool {
		if step == 2 {
			if n, err := h.store.PruneDeleted(ctx, h.nowVal.Unix(), 100); err != nil || n != 1 {
				t.Fatalf("prune: n=%d err=%v", n, err)
			}
		}
		return false
	}

	res := editOf(t, h, f.ID, "FILTER", "L")
	if res.Status != editFailed {
		t.Fatalf("status=%q reason=%q, want failed with no row committed", res.Status, res.Reason)
	}
	if _, ok, _ := h.store.GetFrameByID(ctx, f.ID); ok {
		t.Fatal("setup: the row was to be pruned")
	}
	if v, _ := filterOf(t, h, f.DestPath); v != "L" {
		t.Fatal("the edited file stays in place: a rollback would re-create what no row names")
	}
	if len(openSupersedes(t, h)) != 0 || fileExists(supersedeAsidePath(f.DestPath, f.ArchiveSHA256)) {
		t.Fatal("the swap finished: no intent and no copy set aside remain")
	}
}

// TestEdit_InPlaceCrashWindows (HE-10): a crash after any of the four steps of the
// in-place claim leaves the intent open, so the delete pass never releases the
// device original against the archive mid-swap; startup recovery then lands one
// consistent state — the edit undone after step 1, committed after steps 2–4 —
// whose anchors describe the file at the row's path, and which the delete pass then
// releases on a true premise.
func TestEdit_InPlaceCrashWindows(t *testing.T) {
	for step := 1; step <= 4; step++ {
		t.Run(fmt.Sprintf("crash after step %d", step), func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			confirmDeleteIdentity(h)
			f, body := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
			original := readLocal(t, f.DestPath)

			h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == step }
			if res := editOf(t, h, f.ID, "FILTER", "L"); res.Status != editFailed {
				t.Fatalf("status=%q reason=%q, want failed on the modelled crash", res.Status, res.Reason)
			}
			h.w.crashAfterSupersedeStep = nil
			if !openSupersedes(t, h)[f.DestPath] {
				t.Fatal("the intent must stay open across the crash")
			}

			// Mid-swap, the delete pass defers whatever the file at the path holds.
			h.nowVal = h.nowVal.Add(2 * time.Hour)
			h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
			if !h.deviceHas(editFlatSrc) {
				t.Fatal("a frame mid-swap must never have its device original deleted")
			}
			if h.hasAlert(model.AlertFrameLost) || h.hasAlert(model.AlertDeleteMismatch) {
				t.Fatalf("a crash window may not cost a critical: %v", h.raisedCategories())
			}

			recoverSupersedes(t, h)
			if len(openSupersedes(t, h)) != 0 {
				t.Fatal("recovery must resolve the intent")
			}
			got := mustGetFrame(t, h, f.ID)
			if got.SHA256 != sha256hex(body) || got.State != model.StateVerified || got.PulledAt != f.PulledAt {
				t.Fatalf("an edit never moves the device anchor or lifecycle: sha=%q state=%s pulled_at=%d", got.SHA256, got.State, got.PulledAt)
			}
			if fileExists(supersedeAsidePath(f.DestPath, f.ArchiveSHA256)) {
				t.Fatal("recovery leaves no copy set aside")
			}
			if step == 1 {
				if got.EditSHA256 != "" || got.Filter != "" || !bytes.Equal(readLocal(t, got.DestPath), original) {
					t.Fatal("a crash before the edited file was installed rolls back to the unedited archive")
				}
			} else if got.EditSHA256 != got.ArchiveSHA256 || got.Filter != "L" || len(got.HeaderEdits) != 1 {
				t.Fatalf("a crash after the install recovers to the committed edit: %+v", got)
			}
			wantArchiveConsistent(t, h, got)

			h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
			if h.deviceHas(editFlatSrc) {
				t.Fatal("once recovered, the frame is released against the archive it names")
			}
			if h.hasAlert(model.AlertFrameLost) || h.hasAlert(model.AlertDeleteMismatch) {
				t.Fatalf("recovery may not cost a critical: %v", h.raisedCategories())
			}
		})
	}
}

// TestRecoverSupersedeIntent_EditKind (HE-10): startup recovery commits an
// edit-kind intent through the edit commit — archive anchors, overlay and filter on
// the row — and writes none of the columns an edit has no reason to change, so a
// state, grace anchor or defer count that moved after the intent was written
// survives the replay.
func TestRecoverSupersedeIntent_EditKind(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)

	h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == 2 }
	if res := editOf(t, h, f.ID, "FILTER", "L"); res.Status != editFailed {
		t.Fatalf("status=%q, want the modelled crash", res.Status)
	}
	h.w.crashAfterSupersedeStep = nil
	intents, err := h.store.ListSupersedeIntents(ctx)
	if err != nil || len(intents) != 1 || intents[0].Kind != store.SupersedeEdit {
		t.Fatalf("intents=%+v err=%v, want one edit-kind intent", intents, err)
	}
	in := intents[0]
	if e, ok := in.HeaderEdits.Lookup("FILTER"); !ok || e.Value != "L" || in.Filter != "L" || in.EditSHA256 != in.ArchiveSHA256 {
		t.Fatalf("the intent carries the overlay its commit writes: %+v", in)
	}

	// Columns the edit commit must not write move after the intent was recorded.
	moved := mustGetFrame(t, h, f.ID)
	moved.State, moved.DeletedAt, moved.PulledAt = model.StateDeleted, h.nowVal.Unix(), f.PulledAt+100
	if err := h.store.UpdateFrame(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeferDelete(ctx, f.ID); err != nil {
		t.Fatal(err)
	}

	recoverSupersedes(t, h)
	got := mustGetFrame(t, h, f.ID)
	if got.ArchiveSHA256 != in.ArchiveSHA256 || got.ArchiveSize != in.ArchiveSize || got.EditSHA256 != in.ArchiveSHA256 ||
		got.Filter != "L" || len(got.HeaderEdits) != 1 {
		t.Fatalf("recovery must commit the edit: %+v", got)
	}
	if got.State != model.StateDeleted || got.PulledAt != f.PulledAt+100 || got.DeleteDeferCount != 1 ||
		got.SHA256 != f.SHA256 || got.Size != f.Size || got.Mtime != f.Mtime {
		t.Fatalf("recovery wrote a column the edit commit omits: state=%s pulled_at=%d defer=%d sha=%q",
			got.State, got.PulledAt, got.DeleteDeferCount, got.SHA256)
	}
	wantArchiveConsistent(t, h, got)
}

// TestEdit_ObjectTakesFilingPath (HE-10): an edit including OBJECT takes the filing
// path even when its route keeps the folder, because the target derives the night
// ordinal and the panel columns, which only the filing path writes.
func TestEdit_ObjectTakesFilingPath(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Templates.Light = "<OTA>/<SESSION>/NIGHT_<n>"
	f, _ := fileFilterless(t, h, editLightSrc, "Light", freeSlotCards)
	swapped := false
	h.w.crashAfterSupersedeStep = func(int, string) bool { swapped = true; return false }

	got := mustEdit(t, h, f.ID, "OBJECT", "M31")
	if swapped {
		t.Fatal("an OBJECT edit must never supersede in place")
	}
	if got.Target != "M31" || got.DestPath == f.DestPath || fileExists(f.DestPath) {
		t.Fatalf("target=%q dest=%q (old %q present %v)", got.Target, got.DestPath, f.DestPath, fileExists(f.DestPath))
	}
	wantArchiveConsistent(t, h, got)

	pv := h.w.previewEdit(context.Background(), []int64{f.ID}, "OBJECT", "NGC 7000")
	if len(pv) != 1 || !pv[0].Eligible || pv[0].InPlace {
		t.Fatalf("an OBJECT preview is never in place: %+v", pv)
	}
}

// TestEdit_ScopeGuards: the edit operation's scope (design header-edits §9.2) and
// its no-op rules, each reported skipped with nothing touched.
func TestEdit_ScopeGuards(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	mustEdit(t, h, f.ID, "FILTER", "L")

	for _, tc := range []struct {
		key, value, reason string
	}{
		{"EXPTIME", "30", "not an editable header keyword"},
		{"FILTER", "Hα", "printable ASCII"},
		{"FILTER", "  L ", "equals the current filter"},
		{"OBJECT", revTarget, "equals the current target"},
	} {
		if res := editOf(t, h, f.ID, tc.key, tc.value); res.Status != editSkipped || !strings.Contains(res.Reason, tc.reason) {
			t.Errorf("%s=%q: status=%q reason=%q, want skipped (%s)", tc.key, tc.value, res.Status, res.Reason, tc.reason)
		}
	}
	held := mustGetFrame(t, h, f.ID)
	held.State = model.StateHeld
	if err := h.store.UpdateFrame(ctx, &held); err != nil {
		t.Fatal(err)
	}
	if res := editOf(t, h, f.ID, "FILTER", "R"); res.Status != editSkipped || !strings.Contains(res.Reason, "not editable") {
		t.Fatalf("held: status=%q reason=%q", res.Status, res.Reason)
	}
}

// TestPreviewEdit_Filter: the FILTER preview reports the current value and its
// absence, the commit apply will use, and the rig's spellings that differ from the
// value only in case; it routes what apply routes.
func TestPreviewEdit_Filter(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	g, _ := fileFilterless(t, h, "Autorun/Flat/Flat_0002.fit", "Flat", freeSlotCards+1)
	mustEdit(t, h, g.ID, "FILTER", "HA")

	pv := h.w.previewEdit(ctx, []int64{f.ID, g.ID}, "FILTER", "Ha")
	if len(pv) != 2 {
		t.Fatalf("previews=%+v", pv)
	}
	p := pv[0]
	if !p.Eligible || !p.InPlace || p.NewPath != f.DestPath || p.CurrentPresent || p.CurrentValue != "" {
		t.Fatalf("filterless flat: %+v", p)
	}
	if len(p.ExistingSpellings) != 1 || p.ExistingSpellings[0] != "HA" {
		t.Fatalf("existing_spellings=%v, want [HA]", p.ExistingSpellings)
	}
	if q := pv[1]; !q.Eligible || !q.CurrentPresent || q.CurrentValue != "HA" {
		t.Fatalf("edited flat: %+v", q)
	}
	if same := h.w.previewEdit(ctx, []int64{g.ID}, "FILTER", "HA"); same[0].Eligible || !strings.Contains(same[0].Reason, "equals the current filter") {
		t.Fatalf("no-op preview: %+v", same[0])
	}

	filterInFolderTemplate(h)
	pv = h.w.previewEdit(ctx, []int64{f.ID}, "FILTER", "L")
	if pv[0].InPlace || !pv[0].Eligible {
		t.Fatalf("a template naming <FILTER> re-files: %+v", pv[0])
	}
	got := mustEdit(t, h, f.ID, "FILTER", "L")
	if got.DestPath != pv[0].NewPath {
		t.Fatalf("apply filed to %q, preview said %q", got.DestPath, pv[0].NewPath)
	}
	if bad := h.w.previewEdit(ctx, []int64{f.ID}, "GAIN", "1"); bad[0].Eligible || !strings.Contains(bad[0].Reason, "not an editable") {
		t.Fatalf("unlisted key preview: %+v", bad[0])
	}
}

// TestRefile_FilterlessFrameRoutesAsFiled (design header-edits §9.1): re-file
// rebuilds a filterless frame's classification with FILTER absent, as the live
// pull classified it, so under a template naming <FILTER> it routes to the folder it
// was filed in rather than reading the empty stored filter as a present value that
// renders the segment empty and quarantines.
func TestRefile_FilterlessFrameRoutesAsFiled(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.cfg.Templates.Light = "<TARGET>/<FILTER>/<OTA>/<SESSION>/NIGHT_<n>"
	f, _ := fileFilterless(t, h, editLightSrc, "Light", freeSlotCards)

	pv, err := h.w.previewRefile(ctx, refileScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Groups) != 0 || len(pv.Refusals) != 0 {
		t.Fatalf("a filterless frame must route where its pull filed it: groups=%+v refusals=%+v", pv.Groups, pv.Refusals)
	}
	cls, opts := h.w.refileRouteInputs(h.cfg, f)
	dir, err := h.w.refileDestDir(h.cfg, h.profile, cls, opts)
	if err != nil || dir != filepath.Dir(f.DestPath) {
		t.Fatalf("re-file routes to %q (err %v), filed under %q", dir, err, filepath.Dir(f.DestPath))
	}
}

// TestEdit_InPlaceUnlinkFailureHoldsTheOriginal (design header-edits §8): when step
// 4 cannot remove the copy set aside, the edit has landed — every row describes the
// file in place — but the intent stays open, so the edit reports edited with a
// reason saying the device original is held until the next restart's recovery.
func TestEdit_InPlaceUnlinkFailureHoldsTheOriginal(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	aside := supersedeAsidePath(f.DestPath, f.ArchiveSHA256)
	h.lfs.RemoveErr = func(path string) error {
		if path == aside {
			return os.ErrPermission
		}
		return nil
	}

	res := editOf(t, h, f.ID, "FILTER", "L")
	if res.Status != editEdited || !strings.Contains(res.Reason, "held until the next restart") {
		t.Fatalf("status=%q reason=%q, want edited with the held-original reason", res.Status, res.Reason)
	}
	if !h.logContains("level=ERROR") || !h.logContains("supersede intent is still open") {
		t.Fatal("the held original must be logged at error")
	}
	got := mustGetFrame(t, h, f.ID)
	if got.Filter != "L" || got.EditSHA256 != got.ArchiveSHA256 {
		t.Fatalf("the edit committed: %+v", got)
	}
	wantArchiveConsistent(t, h, got)
	if !openSupersedes(t, h)[f.DestPath] {
		t.Fatal("the intent stays open until recovery removes the leftover")
	}

	h.lfs.RemoveErr = nil
	recoverSupersedes(t, h)
	if fileExists(aside) || len(openSupersedes(t, h)) != 0 {
		t.Fatal("recovery removes the leftover and resolves the intent")
	}
	wantArchiveConsistent(t, h, mustGetFrame(t, h, f.ID))
}

// TestEdit_InPlaceRefusesACrossRigSharer (design header-edits §8): the in-place
// commit reaches only this rig's rows, so an archive another rig's frame also
// names is refused, as skipped, with the archive untouched.
func TestEdit_InPlaceRefusesACrossRigSharer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileFilterless(t, h, editFlatSrc, "Flat", freeSlotCards)
	other := &model.Frame{
		RigID: "rig-02", SourceShare: emmc, SourcePath: "Autorun/Flat/other_0001.fit",
		SHA256: "another-rigs-device-copy", Size: f.Size, Mtime: f.Mtime,
		ArchiveSHA256: f.ArchiveSHA256, ArchiveSize: f.ArchiveSize,
		State: model.StateVerified, DestPath: f.DestPath, PulledAt: f.PulledAt,
	}
	if err := h.store.UpsertFrame(ctx, other); err != nil {
		t.Fatalf("seed the other rig's row: %v", err)
	}
	archive := readLocal(t, f.DestPath)

	res := editOf(t, h, f.ID, "FILTER", "L")
	if res.Status != editSkipped || !strings.Contains(res.Reason, "another rig") {
		t.Fatalf("status=%q reason=%q, want skipped over the cross-rig sharer", res.Status, res.Reason)
	}
	if !bytes.Equal(readLocal(t, f.DestPath), archive) || len(openSupersedes(t, h)) != 0 || !stagingEmpty(t, h.dir) {
		t.Fatal("a refused edit leaves the archive, the intents and staging as they were")
	}
	if got := mustGetFrame(t, h, f.ID); got.EditSHA256 != "" || got.Filter != "" {
		t.Fatalf("a refused edit changed the row: %+v", got)
	}
}

// TestEdit_InPlaceCrashWindowsOnDeletedFrame (HE-10): a `deleted` frame's archive is
// the only copy of its exposure, so every crash window of an in-place edit must
// recover to a file at dest_path that the row's archive anchors describe — the
// unedited archive before the install, the edited one after it — and the row must
// stay a tombstone: an edit never resurrects a deleted frame.
func TestEdit_InPlaceCrashWindowsOnDeletedFrame(t *testing.T) {
	for step := 1; step <= 4; step++ {
		t.Run(fmt.Sprintf("crash after step %d", step), func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			f, _ := fileFilterless(t, h, editFlatSrc, "Flat", fullBlockCards)
			original := readLocal(t, f.DestPath)
			del := *f
			del.State, del.DeletedAt = model.StateDeleted, h.nowVal.Unix()
			if err := h.store.UpdateFrame(ctx, &del); err != nil {
				t.Fatal(err)
			}

			h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == step }
			if res := editOf(t, h, f.ID, "FILTER", "L"); res.Status != editFailed {
				t.Fatalf("status=%q reason=%q, want failed on the modelled crash", res.Status, res.Reason)
			}
			h.w.crashAfterSupersedeStep = nil

			recoverSupersedes(t, h)
			if len(openSupersedes(t, h)) != 0 {
				t.Fatal("recovery must resolve the intent")
			}
			got := mustGetFrame(t, h, f.ID)
			if got.State != model.StateDeleted || got.DeletedAt != del.DeletedAt {
				t.Fatalf("an edit keeps a tombstone a tombstone: state=%s deleted_at=%d", got.State, got.DeletedAt)
			}
			if fileExists(supersedeAsidePath(f.DestPath, f.ArchiveSHA256)) {
				t.Fatal("recovery leaves no copy set aside")
			}
			if step == 1 {
				if got.EditSHA256 != "" || !bytes.Equal(readLocal(t, got.DestPath), original) {
					t.Fatal("a crash before the install leaves the unedited archive, the only copy, in place")
				}
			} else if got.EditSHA256 != got.ArchiveSHA256 || got.Filter != "L" {
				t.Fatalf("a crash after the install recovers to the committed edit: %+v", got)
			}
			wantArchiveConsistent(t, h, got)
		})
	}
}

// TestEdit_FromRecordsAnEmptyFilterCardAsPresent: a FILTER card present with an
// empty value is stored on the row as "", like an absent card; the overlay's audit
// record must still say the device copy carried the card (from ""), not that the
// edit inserted it (from null).
func TestEdit_FromRecordsAnEmptyFilterCardAsPresent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	cards := append(filterlessCards(t, "Flat", freeSlotCards), "FILTER  = '        '           / Filter used")
	body := fitstest.Build(cards, fitstest.SyntheticImage(revW, revH))
	f := pullFile(t, h, editFlatSrc, body, 10*time.Minute)
	if f.State != model.StateVerified || f.Filter != "" {
		t.Fatalf("setup: state=%s filter=%q", f.State, f.Filter)
	}
	got := mustEdit(t, h, f.ID, "FILTER", "L")
	e, ok := got.HeaderEdits.Lookup("FILTER")
	if !ok || e.From == nil || *e.From != "" {
		t.Fatalf("from must record the present, empty card: %+v", got.HeaderEdits)
	}
	if v, _ := filterOf(t, h, got.DestPath); v != "L" {
		t.Fatalf("FILTER=%q, want L", v)
	}
	wantArchiveConsistent(t, h, got)
}
