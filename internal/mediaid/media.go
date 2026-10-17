package mediaid

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/macourteau/asiair-commander/internal/model"
)

// MediaMarkerName is the fixed marker file name for the Udisk media-identity
// marker — junk-class by name (internal/filter carries the matching entry) so it
// is never synced and never deleted by cleanup. eMMC needs no media marker: it is
// a single fixed medium and its volume serial is real and stable, unlike Udisk's
// synthetic constant one (H10). The literal lives in internal/model as the single
// source of truth shared with internal/filter.
const MediaMarkerName = model.MediaMarkerName

// Media is the media-identity marker's content: a random 128-bit identity,
// the instance name, and the instance's public-key fingerprint (bare
// lowercase hex, un-hyphenated — collision-proof where instance names are
// not).
type Media struct {
	ID          string `json:"id"`          // 32 lowercase hex characters
	Instance    string `json:"instance"`    // the instance name
	Fingerprint string `json:"fingerprint"` // 24 lowercase hex characters
}

// Generate mints a fresh media marker with a random 128-bit identity.
func Generate(instance, fingerprint string) (*Media, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("generate media identity: %w", err)
	}
	return &Media{ID: hex.EncodeToString(id[:]), Instance: instance, Fingerprint: fingerprint}, nil
}

// Encode renders the marker as its one-line UTF-8 JSON form.
func (m *Media) Encode() []byte {
	b, _ := json.Marshal(m)
	return append(b, '\n')
}

// Parse reads a media marker file's content. A malformed marker parses as an
// error — verification treats it as mismatched (Foreign), never as absent.
func Parse(data []byte) (*Media, error) {
	var m Media
	if err := json.Unmarshal(bytes.TrimSpace(data), &m); err != nil {
		return nil, fmt.Errorf("malformed media-identity marker: %w", err)
	}
	if m.ID == "" {
		return nil, fmt.Errorf("media-identity marker carries no id")
	}
	return &m, nil
}

// Adopt writes m at the origin's media-marker slot and reads it back to
// prove the write took. The caller persists the identity only after Adopt
// succeeds; a failed write or read-back defers the pass rather than acting
// on an unconfirmed marker.
func (m *Media) Adopt(o Origin) error { return m.AdoptNamed(o, MediaMarkerName) }

// AdoptNamed is Adopt with an explicit marker name, so a caller writing a marker
// other than MediaMarkerName (e.g. the NAS-side library marker) can reuse the
// same write-then-read-back proof.
func (m *Media) AdoptNamed(o Origin, markerName string) error {
	if err := o.WriteMarker(markerName, m.Encode()); err != nil {
		return fmt.Errorf("write media-identity marker: %w", err)
	}
	back, err := o.ReadMarker(markerName)
	if err != nil {
		return fmt.Errorf("read back media-identity marker: %w", err)
	}
	parsed, err := Parse(back)
	if err != nil {
		return fmt.Errorf("read back media-identity marker: %w", err)
	}
	if parsed.ID != m.ID {
		return fmt.Errorf("media-identity marker read-back mismatch")
	}
	return nil
}

// Verdict is a media-marker verification outcome; the caller composes it
// with the root's file population to pick the posture (media absent vs.
// media changed).
type Verdict int

const (
	// Match: the media the confirmations were earned against is the media
	// being cleaned. Proceed.
	Match Verdict = iota
	// AbsentMarker: no marker present. Over an empty root this is the
	// verified removed-stick shape (media absent, self-clearing); over a
	// root holding files it is media changed (hold).
	AbsentMarker
	// SelfRecognized: no persisted identity, but the marker's key
	// fingerprint matches the instance's own key — the instance's own
	// marker surviving a local state loss. Accept and re-persist, with a
	// logged warning.
	SelfRecognized
	// Foreign: the marker matches neither the persisted identity nor the
	// instance's key — another agent claims this root, or the media was
	// swapped. Media changed: hold.
	Foreign
	// Unreadable: the marker read failed with a real error (not absence) —
	// the root is unreachable: treat as media absent, self-clearing.
	Unreadable
)

// Verify reads and judges the media marker against the persisted identity,
// with the own-key self-recognition exception.
func Verify(o Origin, persistedID, ownFingerprint string) (Verdict, *Media) {
	return VerifyNamed(o, MediaMarkerName, persistedID, ownFingerprint)
}

// VerifyNamed is Verify with an explicit marker name, so a caller reading a
// marker other than MediaMarkerName (the NAS-side library marker) gets the same
// AbsentMarker/Unreadable/Foreign/Match discrimination.
func VerifyNamed(o Origin, markerName, persistedID, ownFingerprint string) (Verdict, *Media) {
	data, err := o.ReadMarker(markerName)
	if err != nil {
		if isNotExist(err) {
			return AbsentMarker, nil
		}
		return Unreadable, nil
	}
	m, err := Parse(data)
	if err != nil {
		// A present-but-garbled marker is not the recorded identity: media
		// changed, for a human to judge.
		return Foreign, nil
	}
	if persistedID != "" {
		if m.ID == persistedID {
			return Match, m
		}
		return Foreign, m
	}
	// No persisted identity (local state loss): recognition falls to the
	// key fingerprint.
	if ownFingerprint != "" && m.Fingerprint == ownFingerprint {
		return SelfRecognized, m
	}
	return Foreign, m
}
