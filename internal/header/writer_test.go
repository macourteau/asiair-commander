package header

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header/fitstest"
)

// card renders one 80-column card image space-padded from s.
func card(s string) []byte { return fitstest.Card(s) }

// buildFITS assembles a minimal but real FITS file: the given cards followed by
// END, blank-padded to a whole number of 2880-byte header blocks, then the data
// unit zero-padded to a whole number of blocks. This exercises the writer's real
// byte layout (header blocks + a data unit the complement must cover). The bytes
// come from fitstest so that every synthetic FITS file in the repository — these,
// the committed testdata fixture, and the revision pairs other packages build — is
// laid out by one writer.
func buildFITS(cards []string, data []byte) []byte { return fitstest.Build(cards, data) }

// writeFITS writes a fixture file and returns its path.
func writeFITS(t *testing.T, dir, name string, cards []string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buildFITS(cards, data), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return p
}

var baseCards = []string{
	"SIMPLE  =                    T / conforms to FITS standard",
	"BITPIX  =                    8 / bits per data pixel",
	"NAXIS   =                    0 / number of data axes",
	"IMAGETYP= 'Light   '           / Type of image",
	"OBJECT  = 'Moon'               / name of object being imaged",
	"COMMENT   a commentary card the edit must carry through",
	"FILTER  = 'HA      '           / Filter used",
	"COMMENT   a second commentary card, same key as the first",
}

// filterlessCards is baseCards without its FILTER card: the shape a mono camera
// shot without a filter wheel writes (design header-edits §1).
func filterlessCards() []string {
	var out []string
	for _, c := range baseCards {
		if !strings.HasPrefix(c, "FILTER") {
			out = append(out, c)
		}
	}
	return out
}

// fullBlockCards returns n filler cards before baseCards' tail such that, with
// END, the header ends exactly on a block boundary — END in the 36th slot — and
// carries no FILTER card. A growth test needs this shape (design header-edits §11).
func fullBlockCards(t *testing.T) []string {
	t.Helper()
	cards := filterlessCards()
	for i := 0; len(cards)+1 < 2*fitstest.BlockSize/fitstest.CardSize; i++ {
		cards = append(cards, fmt.Sprintf("HISTORY   synthetic history line %d", i))
	}
	if got := fitstest.HeaderBlocks(cards); got != 2 || (len(cards)+1)%36 != 0 {
		t.Fatalf("fixture shape: %d cards in %d blocks; want END in the last slot of block 2", len(cards)+1, got)
	}
	return cards
}

// digestsOf reads path through the package's own data-digest path and returns the
// whole-file and data-unit digests, independent of the writer's reports.
func digestsOf(t *testing.T, path string) (whole, data string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	whole, data, err = StreamDigests(f, nil, nil)
	if err != nil {
		t.Fatalf("digests of %s: %v", path, err)
	}
	return whole, data
}

func rawHeader(t *testing.T, path string) RawHeader {
	t.Helper()
	rh, err := ReadRawHeader(path)
	if err != nil {
		t.Fatalf("ReadRawHeader(%s): %v", path, err)
	}
	return rh
}

// dataUnit returns the bytes of path from its data offset to end of file.
func dataUnit(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b[rawHeader(t, path).DataOffset:]
}

// checkResultAgainstDisk asserts that the writer's reported size, whole digest and
// data digests match what an independent read of the files finds, and that the
// card complements agree.
func checkResultAgainstDisk(t *testing.T, src, dst string, res EditResult) {
	t.Helper()
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if st.Size() != res.OutputSize {
		t.Errorf("OutputSize = %d, dst is %d bytes", res.OutputSize, st.Size())
	}
	whole, dataOut := digestsOf(t, dst)
	_, dataIn := digestsOf(t, src)
	if whole != hex.EncodeToString(res.OutputSHA256[:]) {
		t.Errorf("OutputSHA256 does not match dst at rest")
	}
	if dataIn != hex.EncodeToString(res.DataIn[:]) || dataOut != hex.EncodeToString(res.DataOut[:]) {
		t.Errorf("DataIn/DataOut do not match the data digests at rest")
	}
	if dataIn != dataOut {
		t.Errorf("data digest changed across the edit")
	}
	if res.CardComplementIn != res.CardComplementOut {
		t.Errorf("CardComplementIn != CardComplementOut")
	}
}

// TestEditCards_OnlyEditedCardsChange is HE-3 at the writer: replacing OBJECT and
// FILTER changes exactly those two card images, at their indices, and leaves every
// other card — repeated COMMENT cards included — the padding and the data unit
// byte-identical.
func TestEditCards_OnlyEditedCardsChange(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, fitstest.SyntheticImage(20, 10))
	dst := filepath.Join(dir, "dst.fit")

	res, err := EditCards(src, dst, []CardEdit{{Key: "OBJECT", Value: "NGC 7000"}, {Key: "FILTER", Value: "OIII"}})
	if err != nil {
		t.Fatalf("EditCards: %v", err)
	}
	checkResultAgainstDisk(t, src, dst, res)

	in, out := rawHeader(t, src), rawHeader(t, dst)
	if len(in.Cards) != len(out.Cards) || in.End != out.End || in.DataOffset != out.DataOffset {
		t.Fatalf("layout changed: %d cards END %d off %d -> %d cards END %d off %d",
			len(in.Cards), in.End, in.DataOffset, len(out.Cards), out.End, out.DataOffset)
	}
	wantObject, _ := CanonicalCard("OBJECT", "NGC 7000")
	wantFilter, _ := CanonicalCard("FILTER", "OIII")
	for i := range in.Cards {
		switch i {
		case 4:
			if out.Cards[i] != wantObject {
				t.Errorf("card %d = %q, want canonical OBJECT", i, out.Cards[i][:])
			}
		case 6:
			if out.Cards[i] != wantFilter {
				t.Errorf("card %d = %q, want canonical FILTER", i, out.Cards[i][:])
			}
		default:
			if in.Cards[i] != out.Cards[i] {
				t.Errorf("card %d changed: %q -> %q", i, in.Cards[i][:], out.Cards[i][:])
			}
		}
	}
	if !bytes.Equal(in.Padding, out.Padding) {
		t.Errorf("padding after END changed")
	}
	if !bytes.Equal(dataUnit(t, src), dataUnit(t, dst)) {
		t.Errorf("data unit changed")
	}

	h, err := New().ReadHeader(dst)
	if err != nil {
		t.Fatalf("ReadHeader(dst): %v", err)
	}
	if got, _ := h.Str("OBJECT"); got != "NGC 7000" {
		t.Errorf("OBJECT = %q, want %q", got, "NGC 7000")
	}
	if got, _ := h.Str("FILTER"); got != "OIII" {
		t.Errorf("FILTER = %q, want %q", got, "OIII")
	}
}

// TestEditCards_ReplaceOnlyKeepsNonBlankPadding: a replace-only edit does not look
// at the bytes after END and carries them through untouched, so a frame
// retargeting accepts stays editable whatever its padding holds (design
// header-edits §4.1).
func TestEditCards_ReplaceOnlyKeepsNonBlankPadding(t *testing.T) {
	dir := t.TempDir()
	b := buildFITS(baseCards, []byte("data"))
	b[(len(baseCards)+1)*cardSize+3] = 'X' // a stray byte in the slot after END
	src := filepath.Join(dir, "src.fit")
	if err := os.WriteFile(src, b, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.fit")
	res, err := EditCards(src, dst, []CardEdit{{Key: "FILTER", Value: "L"}})
	if err != nil {
		t.Fatalf("EditCards: %v", err)
	}
	checkResultAgainstDisk(t, src, dst, res)
	if in, out := rawHeader(t, src), rawHeader(t, dst); !bytes.Equal(in.Padding, out.Padding) {
		t.Errorf("non-blank padding was not carried through verbatim")
	}
}

// TestEditCards_InsertWithFreeSlot is HE-4's first half: inserting FILTER into a
// header with free slots after END keeps the file length and the data digest, puts
// FILTER where END was and END right after it.
func TestEditCards_InsertWithFreeSlot(t *testing.T) {
	dir := t.TempDir()
	cards := filterlessCards()
	src := writeFITS(t, dir, "src.fit", cards, fitstest.SyntheticImage(30, 20))
	dst := filepath.Join(dir, "dst.fit")

	res, err := EditCards(src, dst, []CardEdit{{Key: "FILTER", Value: "  Ha 7nm  "}})
	if err != nil {
		t.Fatalf("EditCards: %v", err)
	}
	checkResultAgainstDisk(t, src, dst, res)

	srcInfo, _ := os.Stat(src)
	if res.OutputSize != srcInfo.Size() {
		t.Errorf("length changed: %d -> %d", srcInfo.Size(), res.OutputSize)
	}
	in, out := rawHeader(t, src), rawHeader(t, dst)
	if out.End != in.End+1 || out.DataOffset != in.DataOffset {
		t.Fatalf("END %d -> %d, data offset %d -> %d; want END+1 and the same offset", in.End, out.End, in.DataOffset, out.DataOffset)
	}
	for i := 0; i < in.End; i++ {
		if in.Cards[i] != out.Cards[i] {
			t.Errorf("card %d changed", i)
		}
	}
	want, _ := CanonicalCard("FILTER", "Ha 7nm")
	if out.Cards[in.End] != want {
		t.Errorf("inserted card = %q, want %q", out.Cards[in.End][:], want[:])
	}
	if out.Cards[out.End] != in.Cards[in.End] {
		t.Errorf("END card changed")
	}
	if len(bytes.Trim(out.Padding, " ")) != 0 {
		t.Errorf("padding after the moved END is not blank")
	}

	h, err := New().ReadHeader(dst)
	if err != nil {
		t.Fatalf("ReadHeader(dst): %v", err)
	}
	if got, _ := h.Str("FILTER"); got != "Ha 7nm" {
		t.Errorf("FILTER = %q, want %q (trimmed, verbatim)", got, "Ha 7nm")
	}
}

// TestEditCards_InsertGrowsFullBlock is HE-4's second half: a header whose END is
// the 36th slot of its last block has nowhere to put a card, so the insertion grows
// the header by exactly one 2880-byte block and shifts the data unit without
// changing it.
func TestEditCards_InsertGrowsFullBlock(t *testing.T) {
	dir := t.TempDir()
	cards := fullBlockCards(t)
	src := writeFITS(t, dir, "src.fit", cards, fitstest.SyntheticImage(30, 20))
	dst := filepath.Join(dir, "dst.fit")

	res, err := EditCards(src, dst, []CardEdit{{Key: "OBJECT", Value: "M 42"}, {Key: "FILTER", Value: "SII"}})
	if err != nil {
		t.Fatalf("EditCards: %v", err)
	}
	checkResultAgainstDisk(t, src, dst, res)

	srcInfo, _ := os.Stat(src)
	if res.OutputSize-srcInfo.Size() != blockSize {
		t.Errorf("grew by %d bytes, want exactly %d", res.OutputSize-srcInfo.Size(), blockSize)
	}
	in, out := rawHeader(t, src), rawHeader(t, dst)
	if out.DataOffset-in.DataOffset != blockSize {
		t.Errorf("data offset moved by %d, want %d", out.DataOffset-in.DataOffset, blockSize)
	}
	if in.End != 71 || out.End != 72 {
		t.Errorf("END index %d -> %d, want 71 -> 72", in.End, out.End)
	}
	want, _ := CanonicalCard("FILTER", "SII")
	if out.Cards[in.End] != want {
		t.Errorf("inserted card = %q", out.Cards[in.End][:])
	}
	if out.Cards[out.End] != in.Cards[in.End] {
		t.Errorf("END card changed")
	}
	if len(out.Padding) != blockSize-cardSize || len(bytes.Trim(out.Padding, " ")) != 0 {
		t.Errorf("grown block is not END followed by 35 blank cards")
	}
	if !bytes.Equal(dataUnit(t, src), dataUnit(t, dst)) {
		t.Errorf("data unit changed")
	}
	h, err := New().ReadHeader(dst)
	if err != nil {
		t.Fatalf("ReadHeader(dst): %v", err)
	}
	if got, _ := h.Str("FILTER"); got != "SII" {
		t.Errorf("FILTER = %q, want SII", got)
	}
}

// TestEditCards_InsertOrder: several insertions land before END in the order of
// edits, not in the allowlist's or the alphabet's. The allowlist has one
// insertable key, so the test registers a second for its duration.
func TestEditCards_InsertOrder(t *testing.T) {
	EditableKeys["ZZTEST"] = EditableKey{Insertable: true, normalize: normalizeFilter}
	t.Cleanup(func() { delete(EditableKeys, "ZZTEST") })

	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", filterlessCards(), []byte("data"))
	dst := filepath.Join(dir, "dst.fit")
	res, err := EditCards(src, dst, []CardEdit{{Key: "ZZTEST", Value: "first"}, {Key: "FILTER", Value: "second"}})
	if err != nil {
		t.Fatalf("EditCards: %v", err)
	}
	checkResultAgainstDisk(t, src, dst, res)

	in, out := rawHeader(t, src), rawHeader(t, dst)
	first, _ := CanonicalCard("ZZTEST", "first")
	second, _ := CanonicalCard("FILTER", "second")
	if out.Cards[in.End] != first || out.Cards[in.End+1] != second || out.End != in.End+2 {
		t.Errorf("inserted cards out of edits order: %q, %q, END at %d",
			out.Cards[in.End][:], out.Cards[in.End+1][:], out.End)
	}
}

// TestEditCards_RefusesUnlistedKey is HE-11 at the writer: a key off the allowlist
// refuses, structural and measured keys included, before dst is created.
func TestEditCards_RefusesUnlistedKey(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("data"))
	for _, key := range []string{"IMAGETYP", "SIMPLE", "NAXIS1", "DATE-OBS", "COMMENT", "", "END"} {
		dst := filepath.Join(dir, "out-"+key+".fit")
		if _, err := EditCards(src, dst, []CardEdit{{Key: key, Value: "x"}}); err == nil {
			t.Errorf("EditCards(%q) = nil error, want refusal", key)
		}
		assertNoFile(t, dst)
	}
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists after a refusal (stat err %v); refusals must precede writing", filepath.Base(path), err)
	}
}

// TestEditCards_Refusals covers every other refusal of design header-edits §4.1;
// each must leave dst uncreated.
func TestEditCards_Refusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := write("base.fit", buildFITS(baseCards, []byte("data")))
	filterless := write("filterless.fit", buildFITS(filterlessCards(), []byte("data")))

	dirtyPad := buildFITS(filterlessCards(), []byte("data"))
	dirtyPad[blockSize-1] = 'X' // the last byte of the END block's padding
	dirty := write("dirtypad.fit", dirtyPad)

	noEnd := write("noend.fit", bytes.Repeat(fitstest.Card("HISTORY   no end here"), maxHeaderBlocks*cardsPerBlock+1))

	// A header filling all maxHeaderBlocks with END in the very last slot: an
	// insertion would push END past the reader's budget.
	var maxed []string
	for len(maxed)+1 < maxHeaderBlocks*cardsPerBlock {
		maxed = append(maxed, "HISTORY   filler")
	}
	atBudget := write("atbudget.fit", buildFITS(maxed, []byte("data")))

	dupObject := write("dupobj.fit", buildFITS(append(append([]string{}, baseCards...), "OBJECT  = 'Mars'"), []byte("data")))
	dupFilter := write("dupfilter.fit", buildFITS(append(append([]string{}, baseCards...), "FILTER  = 'L'"), []byte("data")))
	noObject := write("noobj.fit", buildFITS([]string{"SIMPLE  =                    T", "BITPIX  =                    8"}, []byte("data")))
	commentaryObject := write("commentary.fit", buildFITS([]string{"SIMPLE  =                    T", "OBJECT    this is commentary, not a value card"}, []byte("data")))

	cases := []struct {
		name  string
		src   string
		edits []CardEdit
	}{
		{"no edits", base, nil},
		{"duplicate key", base, []CardEdit{{"OBJECT", "M31"}, {"OBJECT", "M33"}}},
		{"duplicate key differing in case", base, []CardEdit{{"FILTER", "L"}, {"filter", "R"}}},
		{"OBJECT empty after sanitize", base, []CardEdit{{"OBJECT", "."}}},
		{"OBJECT over 60 runes", base, []CardEdit{{"OBJECT", strings.Repeat("X", maxTargetLen+1)}}},
		{"OBJECT card does not fit", base, []CardEdit{{"OBJECT", strings.Repeat("'", maxTargetLen)}}},
		{"FILTER empty", base, []CardEdit{{"FILTER", ""}}},
		{"FILTER blank", base, []CardEdit{{"FILTER", "   "}}},
		{"FILTER over 60 characters", base, []CardEdit{{"FILTER", strings.Repeat("R", maxFilterLen+1)}}},
		{"FILTER non-ASCII", base, []CardEdit{{"FILTER", "Hα"}}},
		{"FILTER control character", base, []CardEdit{{"FILTER", "H\tA"}}},
		{"FILTER card does not fit", base, []CardEdit{{"FILTER", strings.Repeat("'", maxFilterLen)}}},
		{"two OBJECT value cards", dupObject, []CardEdit{{"OBJECT", "M31"}}},
		{"two FILTER value cards", dupFilter, []CardEdit{{"FILTER", "L"}}},
		{"OBJECT is never inserted", noObject, []CardEdit{{"OBJECT", "M31"}}},
		{"commentary OBJECT is not a value card", commentaryObject, []CardEdit{{"OBJECT", "M31"}}},
		{"insertion over non-blank padding", dirty, []CardEdit{{"FILTER", "L"}}},
		{"no END within the block budget", noEnd, []CardEdit{{"OBJECT", "M31"}}},
		{"growth past the block budget", atBudget, []CardEdit{{"FILTER", "L"}}},
		{"bad value refuses the whole edit", filterless, []CardEdit{{"FILTER", "L"}, {"OBJECT", ""}}},
	}
	for i, c := range cases {
		dst := filepath.Join(dir, fmt.Sprintf("out-%d.fit", i))
		if _, err := EditCards(c.src, dst, c.edits); err == nil {
			t.Errorf("%s: EditCards = nil error, want refusal", c.name)
		}
		assertNoFile(t, dst)
	}
}

// TestEditCards_FilterLimitsAccepted pins the edges of the FILTER rule that must
// pass: exactly 60 characters, and every printable ASCII character including the
// quote, which the canonical card escapes.
func TestEditCards_FilterLimitsAccepted(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", filterlessCards(), []byte("data"))
	for i, v := range []string{strings.Repeat("R", maxFilterLen), " !\"#$%&'()*+,-./09:;<=>?@AZ[\\]^_`az{|}~"} {
		dst := filepath.Join(dir, fmt.Sprintf("out-%d.fit", i))
		if _, err := EditCards(src, dst, []CardEdit{{Key: "FILTER", Value: v}}); err != nil {
			t.Fatalf("EditCards(FILTER=%q): %v", v, err)
		}
		h, err := New().ReadHeader(dst)
		if err != nil {
			t.Fatalf("ReadHeader: %v", err)
		}
		if got, _ := h.Str("FILTER"); got != strings.TrimSpace(v) {
			t.Errorf("FILTER = %q, want %q", got, strings.TrimSpace(v))
		}
	}
}

// TestEditCards_ObjectSanitizedAndQuoted: OBJECT is written sanitised, min-padded,
// with an embedded quote doubled, and reads back as the sanitised value.
func TestEditCards_ObjectSanitizedAndQuoted(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("data"))
	for i, c := range []struct{ in, want string }{
		{"M31", "M31"},
		{"Barnard's Loop", "Barnard's Loop"},
		{"Sh2-101: Tulip.", "Sh2-101_ Tulip"},
	} {
		dst := filepath.Join(dir, fmt.Sprintf("out-%d.fit", i))
		if _, err := EditCards(src, dst, []CardEdit{{Key: "OBJECT", Value: c.in}}); err != nil {
			t.Fatalf("EditCards(OBJECT=%q): %v", c.in, err)
		}
		want, _ := buildObjectCard(c.want)
		if got := rawHeader(t, dst).Cards[4]; got != want {
			t.Errorf("OBJECT=%q: card %q, want %q", c.in, got[:], want[:])
		}
		h, _ := New().ReadHeader(dst)
		if got, _ := h.Str("OBJECT"); got != c.want {
			t.Errorf("OBJECT=%q reads back %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReadRawHeader reads card images through END, the padding after it, and the
// data offset DataOffset reports, and refuses a header with no END or one cut off
// inside the block holding END.
func TestReadRawHeader(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("data"))
	rh := rawHeader(t, src)
	if rh.End != len(baseCards) || len(rh.Cards) != rh.End+1 {
		t.Fatalf("End = %d with %d cards, want %d with %d", rh.End, len(rh.Cards), len(baseCards), len(baseCards)+1)
	}
	for i, c := range baseCards {
		if !bytes.Equal(rh.Cards[i][:], fitstest.Card(c)) {
			t.Errorf("card %d = %q, want %q", i, rh.Cards[i][:], c)
		}
	}
	if !bytes.Equal(rh.Cards[rh.End][:], fitstest.Card("END")) {
		t.Errorf("last card is not END")
	}
	if want := blockSize - (rh.End+1)*cardSize; len(rh.Padding) != want {
		t.Errorf("padding is %d bytes, want %d", len(rh.Padding), want)
	}
	off, err := DataOffset(src)
	if err != nil || off != rh.DataOffset {
		t.Errorf("DataOffset = %d (%v), RawHeader.DataOffset = %d", off, err, rh.DataOffset)
	}

	full := writeFITS(t, dir, "full.fit", fullBlockCards(t), []byte("data"))
	frh := rawHeader(t, full)
	if frh.End != 71 || len(frh.Padding) != 0 || frh.DataOffset != 2*blockSize {
		t.Errorf("full block: End %d padding %d offset %d", frh.End, len(frh.Padding), frh.DataOffset)
	}

	noEnd := filepath.Join(dir, "noend.fit")
	if err := os.WriteFile(noEnd, bytes.Repeat(fitstest.Card("HISTORY   x"), maxHeaderBlocks*cardsPerBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRawHeader(noEnd); err == nil {
		t.Errorf("ReadRawHeader with no END = nil error")
	}
	cut := filepath.Join(dir, "cut.fit")
	b := buildFITS(baseCards, nil)
	if err := os.WriteFile(cut, b[:(len(baseCards)+1)*cardSize], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRawHeader(cut); err == nil {
		t.Errorf("ReadRawHeader of a header cut inside the END block = nil error")
	}
}

// TestBuildObjectCard_GoldenBytes freezes the canonical OBJECT card image byte-for-
// byte. Re-apply (design routing-and-filing §9.4) and restore (design
// routing-and-filing §9.4) reproduce this card and require the result to hash back
// to the stored edit_sha256, so any change to the layout — the "OBJECT = '"
// prefix, the min-pad, or the closing quote — would break every
// previously-retargeted frame. The expected images use hardcoded pad counts (not the
// production constants) so a change to those constants fails here loudly.
func TestBuildObjectCard_GoldenBytes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []byte
	}{
		// "M31" → 5 blank pad chars (min 8 value chars), closing quote at column 20.
		{"short", "M31", card("OBJECT  = '" + "M31" + strings.Repeat(" ", 5) + "'")},
		// "O'Ryan" → escaped "O''Ryan" (7 chars) + 1 pad char, closing quote at column 20.
		{"embedded-quote", "O'Ryan", card("OBJECT  = '" + "O''Ryan" + strings.Repeat(" ", 1) + "'")},
	}
	for _, c := range cases {
		got, err := buildObjectCard(c.value)
		if err != nil {
			t.Fatalf("%s: buildObjectCard(%q): %v", c.name, c.value, err)
		}
		if !bytes.Equal(got[:], c.want) {
			t.Errorf("%s: buildObjectCard(%q) golden mismatch\n got %q\nwant %q",
				c.name, c.value, string(got[:]), string(c.want))
		}
	}
}

// TestCanonicalCard_GoldenBytes freezes the generic canonical card layout for a key
// other than OBJECT (design header-edits §4.1): a re-apply must reproduce an edited
// archive byte for byte, so the FILTER card is as frozen as the OBJECT one. It also
// pins the refusal of a key that is not a FITS keyword.
func TestCanonicalCard_GoldenBytes(t *testing.T) {
	cases := []struct {
		key, value string
		want       []byte
	}{
		{"FILTER", "HA", card("FILTER  = '" + "HA" + strings.Repeat(" ", 6) + "'")},
		{"FILTER", "Ha 7nm 'Narrow'", card("FILTER  = '" + "Ha 7nm ''Narrow''" + "'")},
	}
	for _, c := range cases {
		got, err := CanonicalCard(c.key, c.value)
		if err != nil {
			t.Fatalf("CanonicalCard(%q, %q): %v", c.key, c.value, err)
		}
		if !bytes.Equal(got[:], c.want) {
			t.Errorf("CanonicalCard(%q, %q) golden mismatch\n got %q\nwant %q", c.key, c.value, string(got[:]), string(c.want))
		}
	}
	for _, key := range []string{"", "filter", "TOOLONGKEY", "BAD KEY", "A=B"} {
		if _, err := CanonicalCard(key, "x"); err == nil {
			t.Errorf("CanonicalCard(%q) = nil error, want refusal", key)
		}
	}
}
