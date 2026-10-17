// Package demofiles is the contract between tools/seeddemo, which writes the
// store rows, and the `-tags demo` build of Commander, which puts the matching
// files on its fake devices (cmd/asiair-commander/demo.go). Together they stage
// the device file browser for screenshots: Preview trees on both rigs that share
// a file name (a copy conflict), and a frame under the watched Autorun folder
// whose store row is held, so the delete dialog's unarchived count is fixed. It
// is NOT part of the product: only the seeder and the demo build import it.
package demofiles

import "time"

// The rig GUIDs seeddemo gives its two rigs in live mode. The demo resolver keys
// one fake device per GUID, so a staged file names the fake it belongs on.
const (
	RigFRA  = "demo-fra"
	RigNewt = "demo-newt"
)

// Share is the share every staged file lives on, the one the rigs watch.
const Share = "EMMC Images"

// FullName is a full-length ASIAIR frame name, the longest unbroken string the
// panel renders, so the mobile overflow check has something real to wrap.
const FullName = "Light_M42_300.0s_Bin1_2600MM_gain100_20261007-231502_-10.0C_0001.fit"

// File is one staged device file.
type File struct {
	// Rig is the GUID of the fake the file is on; empty puts it on both rigs.
	Rig   string
	Path  string
	Size  int
	Mtime time.Time
}

// base is a fixed night, so a store row and its device file agree on mtime
// across separate seeder and server runs.
var base = time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)

// HeldFrame is the frame under the watched Autorun that seeddemo records as
// held on both rigs. decide() never pulls a held row whose size and mtime match
// the listing, and the delete pass never selects one, so the file stays put and
// stays unarchived however long the live demo runs.
var HeldFrame = File{
	Path:  "Autorun/Light/M42/" + FullName,
	Size:  192 << 10,
	Mtime: base,
}

// Files is every staged file, HeldFrame included.
var Files = []File{
	HeldFrame,
	// The same frame under Preview on both rigs, with different content, so
	// copying it across shows the conflict table with two distinct versions.
	{Rig: RigFRA, Path: "Preview/M42/" + FullName, Size: 160 << 10, Mtime: base.Add(4 * time.Minute)},
	{Rig: RigNewt, Path: "Preview/M42/" + FullName, Size: 128 << 10, Mtime: base.Add(-26 * time.Hour)},
	{Path: "Preview/M42/Light_M42_300.0s_Bin1_2600MM_gain100_20261007-231502_-10.0C_0001_thn.jpg", Size: 24 << 10, Mtime: base.Add(4 * time.Minute)},
	// Only on the FRA300, so a copy of the folder has something new to copy too.
	{Rig: RigFRA, Path: "Preview/M42/Light_M42_300.0s_Bin1_2600MM_gain100_20261007-232012_-10.0C_0002.fit", Size: 160 << 10, Mtime: base.Add(9 * time.Minute)},
	{Rig: RigFRA, Path: "Preview/Stack_M42_20261007.jpg", Size: 96 << 10, Mtime: base.Add(40 * time.Minute)},
	{Rig: RigNewt, Path: "Preview/IC 1805/Light_IC 1805_180.0s_Bin1_533MC_gain100_20261006-222210_-5.0C_0012.fit", Size: 96 << 10, Mtime: base.Add(-25 * time.Hour)},
}
