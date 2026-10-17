package mediaid

import (
	"errors"
	"path"
	"testing"
)

func TestGenerateEncodeParseRoundTrip(t *testing.T) {
	m, err := Generate("rig-1", "abcdef012345abcdef012345")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(m.ID) != 32 {
		t.Fatalf("ID length = %d, want 32 (128 bits of hex)", len(m.ID))
	}

	back, err := Parse(m.Encode())
	if err != nil {
		t.Fatalf("Parse(Encode()): %v", err)
	}
	if *back != *m {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", *back, *m)
	}
}

func TestGenerateIsRandom(t *testing.T) {
	a, err := Generate("rig-1", "fp")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate("rig-1", "fp")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("two Generate calls produced the same id: %s", a.ID)
	}
}

func TestParseMalformed(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"not json", []byte("not json at all\n")},
		{"empty id", []byte(`{"id":"","instance":"x","fingerprint":"y"}` + "\n")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(c.data); err == nil {
				t.Fatal("Parse: expected error, got nil")
			}
		})
	}
}

func TestMediaAdoptThenVerifyMatch(t *testing.T) {
	o := newFakeOrigin()
	m, err := Generate("rig-1", "abcdef012345abcdef012345")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := m.Adopt(o); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	verdict, got := Verify(o, m.ID, "")
	if verdict != Match {
		t.Fatalf("Verify verdict = %v, want Match", verdict)
	}
	if got == nil || got.ID != m.ID {
		t.Fatalf("Verify marker = %+v, want ID %s", got, m.ID)
	}
}

func TestMediaAdoptWriteFailure(t *testing.T) {
	o := newFakeOrigin()
	o.writeErr[MediaMarkerName] = errors.New("share unavailable")
	m, err := Generate("rig-1", "fp")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := m.Adopt(o); err == nil {
		t.Fatal("Adopt: expected error on write failure, got nil")
	}
}

func TestVerifyNoMarkerAbsent(t *testing.T) {
	o := newFakeOrigin()
	verdict, got := Verify(o, "some-persisted-id", "")
	if verdict != AbsentMarker {
		t.Fatalf("Verify verdict = %v, want AbsentMarker", verdict)
	}
	if got != nil {
		t.Fatalf("Verify marker = %+v, want nil", got)
	}
}

func TestVerifyForeignID(t *testing.T) {
	o := newFakeOrigin()
	m, err := Generate("rig-1", "fp")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := m.Adopt(o); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	verdict, got := Verify(o, "some-other-persisted-id", "")
	if verdict != Foreign {
		t.Fatalf("Verify verdict = %v, want Foreign", verdict)
	}
	if got == nil || got.ID != m.ID {
		t.Fatalf("Verify marker = %+v, want the foreign marker itself for logging", got)
	}
}

func TestVerifyForeignGarbledMarker(t *testing.T) {
	o := newFakeOrigin()
	if err := o.WriteMarker(MediaMarkerName, []byte("garbage\n")); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	verdict, got := Verify(o, "some-persisted-id", "")
	if verdict != Foreign {
		t.Fatalf("Verify verdict = %v, want Foreign", verdict)
	}
	if got != nil {
		t.Fatalf("Verify marker = %+v, want nil for an unparseable marker", got)
	}
}

func TestVerifySelfRecognized(t *testing.T) {
	o := newFakeOrigin()
	m, err := Generate("rig-1", "abcdef012345abcdef012345")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := m.Adopt(o); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	// No persisted id (as after local state loss): recognition falls to
	// the instance's own key fingerprint.
	verdict, got := Verify(o, "", "abcdef012345abcdef012345")
	if verdict != SelfRecognized {
		t.Fatalf("Verify verdict = %v, want SelfRecognized", verdict)
	}
	if got == nil || got.ID != m.ID {
		t.Fatalf("Verify marker = %+v, want ID %s", got, m.ID)
	}
}

func TestVerifyNoPersistedIDAndNoFingerprintMatchIsForeign(t *testing.T) {
	o := newFakeOrigin()
	m, err := Generate("rig-1", "abcdef012345abcdef012345")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := m.Adopt(o); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	verdict, _ := Verify(o, "", "a-different-fingerprint")
	if verdict != Foreign {
		t.Fatalf("Verify verdict = %v, want Foreign", verdict)
	}
}

// TestPrefixedOriginRoundTrip proves PrefixedOrigin roots marker access at its
// prefix: a marker adopted through the "ASIAIR" prefix lands at the joined
// subpath, verifies Match through the same prefix, and is invisible at the root.
func TestPrefixedOriginRoundTrip(t *testing.T) {
	store := newFakeMarkerStore()
	asiair := PrefixedOrigin(store, "ASIAIR")
	root := PrefixedOrigin(store, "")

	m, err := Generate("rig-1", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := m.Adopt(asiair); err != nil {
		t.Fatalf("Adopt via ASIAIR prefix: %v", err)
	}

	// The marker landed under ASIAIR/, not at the root.
	if _, ok := store.data[path.Join("ASIAIR", MediaMarkerName)]; !ok {
		t.Fatalf("marker not stored under ASIAIR/; store = %v", store.data)
	}
	if verdict, _ := Verify(root, m.ID, ""); verdict != AbsentMarker {
		t.Fatalf("root Verify = %v, want AbsentMarker (marker lives under ASIAIR/)", verdict)
	}
	if verdict, got := Verify(asiair, m.ID, ""); verdict != Match || got == nil || got.ID != m.ID {
		t.Fatalf("ASIAIR Verify = %v/%+v, want Match with id %s", verdict, got, m.ID)
	}
}

func TestVerifyUnreadable(t *testing.T) {
	o := newFakeOrigin()
	o.readErr[MediaMarkerName] = errors.New("connection reset")

	verdict, got := Verify(o, "some-persisted-id", "")
	if verdict != Unreadable {
		t.Fatalf("Verify verdict = %v, want Unreadable", verdict)
	}
	if got != nil {
		t.Fatalf("Verify marker = %+v, want nil", got)
	}
}
