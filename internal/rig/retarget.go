package rig

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
)

// archiveHash is the single accessor that generalizes every hash comparison in the
// codebase (design routing-and-filing §9.5): "what the file on the NAS should hash
// to." It is a stored value rather than a derivation, because the fallback it used
// to make — EditSHA256 when an overlay exists, SHA256 otherwise — rested on the
// device original hashing to SHA256 for the frame's whole life, which a device-side
// revision falsifies: once a supersede advances SHA256, a derived expectation would
// silently move the archive-side expectation of every frame carrying no overlay.
// Device-side reads keep comparing to f.SHA256, and archive-side reads take their
// length from f.ArchiveSize for the same reason (design frame-lifecycle §11.1).
func archiveHash(f model.Frame) string {
	return f.ArchiveSHA256
}

// nightProjector projects the ordinal apply would issue to each frame of one preview
// batch, without writing: AssignNight is never called from a preview, because a
// previewed-then-abandoned retarget must not permanently spend an ordinal
// (design routing-and-filing §6.5).
//
// It reads the destination folder through the same nightHints the filing path uses
// rather than deriving it a second way — two implementations of that read is exactly
// the divergence the design exists to eliminate — and carries three distinct pieces of
// state, because conflating any two of them breaks the projection.
type nightProjector struct {
	w       *Worker
	cfg     *model.Config
	profile *model.RigProfile
	// target is the scope's target half: the sanitized string, because
	// the OBJECT rule sanitizes before the card is written, so that is what apply
	// re-classifies and keys the ledger on.
	target string

	// folders caches one folder read per (prefix, observing date). The date belongs
	// in the key because the matching count is a function of the date window: a
	// prefix-only key would hand every frame of a multi-date batch the first
	// frame's answer.
	folders map[string]nightHint
	// resolved memoizes ordinals by observing date ALONE. Apply's LookupNight hit is
	// scope-wide, so under a template that varies above the night segment two frames
	// of one date in different prefixes still get one ordinal; a (prefix, date) key
	// would show two. It also makes a repeated id — handleEditApply does not
	// dedupe — cost nothing.
	resolved map[string]projectedNight
	// runningMax is advanced by every arm that takes an ordinal — mints, adoptions
	// and the error arm — because each changes what the next frame of the same apply
	// would be issued. Advancing on an adoption matters when a batch spans several
	// prefixes; within one prefix it costs nothing, since an adopted ordinal is
	// never above that folder's high-water mark.
	runningMax int

	scopeMax   int
	scopeMaxOK bool
}

// projectedNight is one frame's projected ordinal. exact is true only for a date the
// ledger has already assigned: that one cannot move, because an assigned night is
// never renumbered, while an adoption or a mint can be shifted by anything filed
// between the preview and the apply.
type projectedNight struct {
	night int
	exact bool
}

// project resolves the ordinal one light frame would file under, and the raw target
// spellings already filed in the folder that ordinal names.
func (p *nightProjector) project(ctx context.Context, cls model.Classification, opts route.Options) (projectedNight, []string, error) {
	key := route.ObservingDateKey(opts.ObservingDate)
	h, err := p.folderHint(ctx, cls, opts, key)
	if err != nil {
		return projectedNight{}, nil, err
	}
	pn, err := p.ordinal(ctx, key, h)
	if err != nil {
		return projectedNight{}, nil, err
	}
	spellings, err := p.spellings(ctx, h, pn.night)
	if err != nil {
		return projectedNight{}, nil, err
	}
	return pn, spellings, nil
}

// folderHint returns what the frame's destination folder holds, one read per
// (prefix, observing date). The prefix is rendered here only to key the cache —
// rendering is pure and free of I/O — while the read itself stays in nightHints, so
// the preview and the filing path cannot disagree about what a folder holds.
func (p *nightProjector) folderHint(ctx context.Context, cls model.Classification, opts route.Options, dateKey string) (nightHint, error) {
	prefix, _, ok, err := route.NightFolder(cls, *p.profile, opts)
	if err != nil {
		return nightHint{}, err
	}
	if !ok {
		return nightHint{}, nil
	}
	key := prefix + "\x00" + dateKey
	if h, cached := p.folders[key]; cached {
		return h, nil
	}
	h, err := p.w.nightHints(ctx, p.cfg, p.profile, cls, opts)
	if err != nil {
		return nightHint{}, err
	}
	p.folders[key] = h
	return h, nil
}

// ordinal is the three-armed projection of design routing-and-filing §6.5: a date
// the ledger has already assigned keeps its ordinal; a folder holding this date
// lends its own, unless another date in the scope already holds it; otherwise the
// ordinal is apply's own mint formula, the scope maximum included, advanced by what
// this batch has already taken.
func (p *nightProjector) ordinal(ctx context.Context, dateKey string, h nightHint) (projectedNight, error) {
	if pn, ok := p.resolved[dateKey]; ok {
		return pn, nil
	}
	n, assigned, err := p.w.store.LookupNight(ctx, p.target, p.profile.OTA, dateKey)
	if err != nil {
		return projectedNight{}, err
	}
	if assigned {
		pn := projectedNight{night: n, exact: true}
		p.resolved[dateKey] = pn
		return pn, nil
	}
	if !p.scopeMaxOK {
		m, merr := p.w.store.MaxNight(ctx, p.target, p.profile.OTA)
		if merr != nil {
			return projectedNight{}, merr
		}
		p.scopeMax, p.scopeMaxOK = m, true
	}

	night := 0
	if h.Preferred >= 1 {
		held, herr := p.heldByOtherDate(ctx, h.Preferred, dateKey)
		if herr != nil {
			return projectedNight{}, herr
		}
		if !held {
			night = h.Preferred
		}
	}
	if night == 0 {
		night = p.scopeMax
		if h.FolderHigh > night {
			night = h.FolderHigh
		}
		if p.runningMax > night {
			night = p.runningMax
		}
		night++
	}
	if night > p.runningMax {
		p.runningMax = night
	}
	pn := projectedNight{night: night}
	p.resolved[dateKey] = pn
	return pn, nil
}

// heldByOtherDate is AssignNight's rule 2 asked as a read: an ordinal some other
// observing date in this scope already holds is not adoptable, and a projection that
// skipped the rule would state an ordinal apply refuses to issue. The batch's own
// projections are consulted alongside the ledger, because an earlier frame of this
// same apply may already have taken the ordinal — a preview writes nothing, so the
// ledger cannot yet know that.
func (p *nightProjector) heldByOtherDate(ctx context.Context, night int, dateKey string) (bool, error) {
	for date, pn := range p.resolved {
		if pn.night == night && date != dateKey {
			return true, nil
		}
	}
	return p.w.store.NightHeldByOtherDate(ctx, p.target, p.profile.OTA, night, dateKey)
}

// spellings reports the raw OBJECT spellings already filed in the resolved night
// folder, when they differ from the target as it will be WRITTEN — the OBJECT rule
// sanitizes before writing the card, so comparing against the typed string would
// report a spurious divergence for "NGC:7380". Unifying the night does not unify the
// archive: anything grouping by OBJECT, re-file included, still sees two targets.
//
// It reads the resolved folder rather than the prefix, or a plain-target retarget
// under the shipped mosaic template would report every panel's spelling. On the mint
// arm the folder does not exist yet, so the answer is empty by construction rather
// than by a special case.
func (p *nightProjector) spellings(ctx context.Context, h nightHint, night int) ([]string, error) {
	if !h.OK || night < 1 {
		return nil, nil
	}
	name, err := h.NightName(night)
	if err != nil {
		// The folder's own segment cannot be rendered for this ordinal, so there is no
		// folder to read; route.Route re-derives the same quarantine moments later.
		return nil, nil
	}
	lo, hi := folderRange(joinLocal(joinLocal(p.cfg.Roots.Light, h.Prefix), name))
	targets, err := p.w.store.DistinctTargetsUnder(ctx, lo, hi)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range targets {
		if t != p.target {
			out = append(out, t)
		}
	}
	// SELECT DISTINCT has no defined order; the operator reads this list.
	sort.Strings(out)
	return out, nil
}

// parseBinningToken is the inverse of binningToken: it parses "Bin1" → (1,1) and
// "Bin1x2" → (1,2) so the edit preview and re-file can rebuild BinX/BinY from the
// stored column. It reports ok=false for an empty or unparseable token.
func parseBinningToken(tok string) (x, y int64, ok bool) {
	rest, found := strings.CutPrefix(tok, "Bin")
	if !found || rest == "" {
		return 0, 0, false
	}
	if a, b, split := strings.Cut(rest, "x"); split {
		xi, e1 := strconv.ParseInt(a, 10, 64)
		yi, e2 := strconv.ParseInt(b, 10, 64)
		if e1 != nil || e2 != nil {
			return 0, 0, false
		}
		return xi, yi, true
	}
	n, e := strconv.ParseInt(rest, 10, 64)
	if e != nil {
		return 0, 0, false
	}
	return n, n, true
}
