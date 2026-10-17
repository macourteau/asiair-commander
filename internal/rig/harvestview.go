package rig

import (
	"path"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// The harvest view answers, for a share-relative device path, what the rig's
// harvester would do with it, without a worker or a device: the device file
// browser badges, counts and warns from it while the harvester keeps running.
// Every predicate restates a pull-path rule by calling the code that applies it:
// the enabled sources (EnabledSources, as pullPhase merges them), both data
// layouts (resolveDataPrefix, nestedDataPrefix), the walk's junk rules
// (walkPrunesDir, walkDropsFile) and pairListing's preview split
// (isListedPreview).
//
// The ASIAIR prefix and the root segments match case-insensitively: a FAT device
// may list AUTORUN for a configured Autorun, and the harvester's ListDir of the
// configured case finds it there. The share name matches case-insensitively
// too, as Samba resolves it. Store rows are keyed exact-case, so lookups go
// through HarvestStoreKey.

// watchedMatch is where a share-relative path falls under one of a rig's watched
// roots.
type watchedMatch struct {
	// key is path.Join(prefix, root) as listShareRoots builds it: the literal
	// nestedDataPrefix (or none) and the configured root's case. Every listed path,
	// and so every frame row's source path, starts with it.
	key string
	// rest is the path's segments below the root, in the case given.
	rest []string
}

// splitDevicePath splits a share-relative path into its segments; the share root
// ("" or "/") has none.
func splitDevicePath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// hasPrefixFold reports whether segs begins with lead, segment by segment,
// ignoring case.
func hasPrefixFold(segs, lead []string) bool {
	if len(lead) > len(segs) {
		return false
	}
	for i := range lead {
		if !strings.EqualFold(segs[i], lead[i]) {
			return false
		}
	}
	return true
}

// matchWatched finds the first source root on share that p is, or lies under,
// in either layout. Both layouts are accepted because resolveDataPrefix picks one
// per share per cycle and never stores it. The harvest predicates pass
// EnabledSources, the sources the harvester walks; the rename rule passes every
// configured source and the store-kept roots too.
func matchWatched(sources []model.Source, share, p string) (watchedMatch, bool) {
	segs := splitDevicePath(p)
	for _, src := range sources {
		if !strings.EqualFold(src.Share, share) {
			continue
		}
		for _, root := range src.Roots {
			for _, prefix := range [...]string{"", nestedDataPrefix} {
				key := path.Join(prefix, root)
				lead := splitDevicePath(key)
				if hasPrefixFold(segs, lead) {
					return watchedMatch{key: key, rest: segs[len(lead):]}, true
				}
			}
		}
	}
	return watchedMatch{}, false
}

// prunedBelowRoot reports whether the walk would prune any of dirs, the folders
// between a root and an entry. The root and the ASIAIR prefix are never tested:
// the walk starts at path.Join(prefix, root).
func prunedBelowRoot(dirs []string) bool {
	for _, d := range dirs {
		if walkPrunesDir(d) {
			return true
		}
	}
	return false
}

// listedFile reports whether the pull walk on share would list p as a file, and
// whether pairListing would then split it off as a preview.
func listedFile(profile *model.RigProfile, exclude []string, share, p string) (listed, preview bool) {
	m, ok := matchWatched(profile.EnabledSources(), share, p)
	if !ok || len(m.rest) == 0 {
		return false, false
	}
	name := m.rest[len(m.rest)-1]
	if prunedBelowRoot(m.rest[:len(m.rest)-1]) || walkDropsFile(name) {
		return false, false
	}
	return true, isListedPreview(name, exclude)
}

// HarvestCandidate reports whether the pull listing would treat share/path as a
// frame for profile: an enabled source on share, path under one of its roots in
// either layout (flat or under "ASIAIR/"), no junk directory below the root, not
// a junk file, extension not in exclude (cfg.ExcludeExtensions).
func HarvestCandidate(profile *model.RigProfile, exclude []string, share, path string) bool {
	listed, preview := listedFile(profile, exclude, share, path)
	return listed && !preview
}

// HarvestPreview is HarvestCandidate for an excluded-extension file (a preview),
// which the harvester never archives: it deletes it with its frame or sweeps it
// as an orphan.
func HarvestPreview(profile *model.RigProfile, exclude []string, share, path string) bool {
	listed, preview := listedFile(profile, exclude, share, path)
	return listed && preview
}

// WatchedDir reports whether the folder dir is a watched root or lies inside one
// the walk descends into (no junk folder below the root), in either layout.
func WatchedDir(profile *model.RigProfile, share, dir string) bool {
	_, ok := watchedDirIn(profile.EnabledSources(), share, dir)
	return ok
}

// watchedDirIn is WatchedDir over a given source list, answering the matched
// root's key: the walk of that root descends into dir.
func watchedDirIn(sources []model.Source, share, dir string) (key string, ok bool) {
	m, ok := matchWatched(sources, share, dir)
	if !ok || prunedBelowRoot(m.rest) {
		return "", false
	}
	return m.key, true
}

// WatchedRoot reports whether dir is exactly a watched root, in either layout.
func WatchedRoot(profile *model.RigProfile, share, dir string) bool {
	m, ok := matchWatched(profile.EnabledSources(), share, dir)
	return ok && len(m.rest) == 0
}

// UnderWatchedRoot reports whether p is one of sources' roots on share, or lies
// under one, in either layout, segments compared ignoring case. Unlike
// WatchedDir it takes the source list and ignores the walk's junk pruning: it
// answers where a root is, not what the walk descends into.
func UnderWatchedRoot(sources []model.Source, share, p string) bool {
	_, ok := matchWatched(sources, share, p)
	return ok
}

// HoldsWatchedRoot reports whether p is a root of one of sources on share, or a
// strict ancestor of one, in either layout, segments compared ignoring case:
// Udisk "ASIAIR" holds "ASIAIR/Autorun", "Foo" holds the root "Foo/Bar". The
// share root holds every root but is never an entry, so it answers false.
func HoldsWatchedRoot(sources []model.Source, share, p string) bool {
	_, ok := heldRoot(sources, share, p)
	return ok
}

// heldRoot is HoldsWatchedRoot answering the first root p is or holds, as its
// key (the ASIAIR prefix and the configured case).
func heldRoot(sources []model.Source, share, p string) (key string, ok bool) {
	segs := splitDevicePath(p)
	if len(segs) == 0 {
		return "", false
	}
	for _, src := range sources {
		if !strings.EqualFold(src.Share, share) {
			continue
		}
		for _, root := range src.Roots {
			for _, prefix := range [...]string{"", nestedDataPrefix} {
				key := path.Join(prefix, root)
				if hasPrefixFold(splitDevicePath(key), segs) {
					return key, true
				}
			}
		}
	}
	return "", false
}

// RenameRule names the rename refusal a path meets. Rules R3 (the layout
// guard, which needs a device Stat) and R4 (marker names, the name validator's)
// live in devfiles.
type RenameRule string

const (
	// RenameInWatched (R1): the item's folder lies in a watched root, so the
	// harvester keys its frames, previews and folders by the path a rename moves.
	RenameInWatched RenameRule = "watched"
	// RenameReserved (R2): the old or new path is, or holds, a reserved path: a
	// watched root, or a removable share's ASIAIR folder.
	RenameReserved RenameRule = "reserved"
)

// RenameCause is what made a path watched or reserved, tested in this order:
// the sentence a refusal reads as depends on it.
type RenameCause string

const (
	// RenameCauseEnabled: a root of an enabled source, which the Watched badge shows.
	RenameCauseEnabled RenameCause = "enabled"
	// RenameCauseDisabled: a root of a disabled source only. Its rows are kept and
	// it is walked again once enabled, but no badge shows it.
	RenameCauseDisabled RenameCause = "disabled"
	// RenameCauseDataFolder: a removable share's ASIAIR folder, which holds the
	// media marker and the nested layout whatever sources are configured.
	RenameCauseDataFolder RenameCause = "data_folder"
	// RenameCauseStore: only a root no longer configured whose rows the store
	// still keeps in play.
	RenameCauseStore RenameCause = "store"
)

// RenameRefusal is a rename rule's verdict on a path.
type RenameRefusal struct {
	Rule  RenameRule
	Cause RenameCause
	// Root is the watched or reserved path that caused it, share-relative: the
	// ASIAIR prefix and the configured case for a configured root, the derived
	// key for a store-kept one.
	Root string
}

// causeSources is one cause's share of the rename rule's source list.
type causeSources struct {
	cause   RenameCause
	sources []model.Source
}

// renameSourceGroups splits the rename rule's source list by cause, in the
// order the causes are tested. storeRoots are the roots the store keeps in play
// on share, carried as one synthetic source.
func renameSourceGroups(sources []model.Source, storeRoots []string, share string) []causeSources {
	var enabled, disabled []model.Source
	for _, src := range sources {
		if src.Disabled {
			disabled = append(disabled, src)
		} else {
			enabled = append(enabled, src)
		}
	}
	return []causeSources{
		{RenameCauseEnabled, enabled},
		{RenameCauseDisabled, disabled},
		{RenameCauseStore, []model.Source{{Share: share, Roots: storeRoots}}},
	}
}

// renameInWatched is R1 for an item in dir: dir is a watched root or lies in
// one, and the walk descends into it. A folder the walk prunes as junk is not
// watched, and neither is what it holds.
func renameInWatched(sources []model.Source, storeRoots []string, share, dir string) (RenameRefusal, bool) {
	for _, g := range renameSourceGroups(sources, storeRoots, share) {
		if key, ok := watchedDirIn(g.sources, share, dir); ok {
			return RenameRefusal{Rule: RenameInWatched, Cause: g.cause, Root: key}, true
		}
	}
	return RenameRefusal{}, false
}

// renameReserved is R2 for the path p: it is, or holds, a reserved path. A
// removable share's ASIAIR folder ranks after the configured roots, which name
// the root more precisely, and before the store-kept ones, whose sentence would
// leave the data folder unsaid.
func renameReserved(sources []model.Source, storeRoots []string, share, p string) (RenameRefusal, bool) {
	groups := renameSourceGroups(sources, storeRoots, share)
	for _, g := range groups[:2] {
		if key, ok := heldRoot(g.sources, share, p); ok {
			return RenameRefusal{Rule: RenameReserved, Cause: g.cause, Root: key}, true
		}
	}
	if IsRemovableShare(share) && strings.EqualFold(strings.Trim(p, "/"), mediaMarkerPrefix) {
		return RenameRefusal{Rule: RenameReserved, Cause: RenameCauseDataFolder, Root: mediaMarkerPrefix}, true
	}
	if key, ok := heldRoot(groups[2].sources, share, p); ok {
		return RenameRefusal{Rule: RenameReserved, Cause: RenameCauseStore, Root: key}, true
	}
	return RenameRefusal{}, false
}

// RenameFromRefused is the rename rule's verdict on an existing entry, name in
// dir on share, before any new name is known: R1, then R2's old-name half for a
// folder. A file named like a reserved path stays renamable: renaming a stray
// file away repairs the share's listing. sources are every configured source of
// the rig, enabled or disabled; storeRoots are the roots on share the store
// still keeps in play (computed by the caller, so this stays pure).
func RenameFromRefused(sources []model.Source, storeRoots []string, share, dir, name string, isDir bool) (RenameRefusal, bool) {
	if r, ok := renameInWatched(sources, storeRoots, share, dir); ok {
		return r, true
	}
	if !isDir {
		return RenameRefusal{}, false
	}
	return renameReserved(sources, storeRoots, share, path.Join(dir, name))
}

// RenameToRefused is the verdict on renaming an entry in dir on share to
// newName, from the configuration and the store alone: R1, then R2's new-name
// half, which holds for a file or a folder since either standing at a reserved
// path disrupts the harvester.
func RenameToRefused(sources []model.Source, storeRoots []string, share, dir, newName string) (RenameRefusal, bool) {
	if r, ok := renameInWatched(sources, storeRoots, share, dir); ok {
		return r, true
	}
	return renameReserved(sources, storeRoots, share, path.Join(dir, newName))
}

// ReservedPath reports whether a file at p on share would stand where a
// reserved path must go: p is, or holds, a root of one of sources in either
// layout, or is a removable share's ASIAIR folder. root names it as
// RenameRefusal.Root does. It is R2's test over the configuration alone, which
// copy and upload apply to every file they would write: a file can stand at a
// root no longer configured only once that folder is gone, and with it the
// rows the store-kept roots exist for. sources are every configured source of
// the rig, enabled or disabled.
func ReservedPath(sources []model.Source, share, p string) (root string, ok bool) {
	r, ok := renameReserved(sources, nil, share, p)
	return r.Root, ok
}

// HarvestStoreKey rewrites a device path under a watched root to the source path
// the harvester keys its frame row by: the literal ASIAIR prefix and the
// configured root's case, followed by the remaining segments as given (the walk
// keeps listed names verbatim). A path outside every watched root is returned
// unchanged with ok false; it has no row.
func HarvestStoreKey(profile *model.RigProfile, share, p string) (key string, ok bool) {
	m, ok := matchWatched(profile.EnabledSources(), share, p)
	if !ok {
		return p, false
	}
	key = m.key
	for _, seg := range m.rest {
		key = joinPath(key, seg)
	}
	return key, true
}

// ArchivedCopy reports whether row proves a NAS copy of this exact device file
// exists: a proven-archived state (verified or unclassified), not culled on the
// NAS (MissingSeenAt == 0), and the same size and mtime decide compares.
//
// It is deliberately stricter than the pull tally and safelyOnNAS, which stop
// counting culled and dismissed frames so the rig does not freeze at Pulling;
// neither proves a NAS copy exists, so a tool delete must count them. held is
// excluded because its NAS copy is suspect. Every row it accepts is one decide
// skips, so the two disagree only in the conservative direction.
func ArchivedCopy(row *model.Frame, size int64, mtime time.Time) bool {
	return row != nil &&
		row.State.ProvenArchived() &&
		row.MissingSeenAt == 0 &&
		sameListedContent(row, size, mtime)
}
