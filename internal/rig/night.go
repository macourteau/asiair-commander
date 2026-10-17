package rig

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
)

// dateObsBoundLayout renders the date_obs range bounds a folder read compares
// against. It is deliberately NOT RFC3339Nano: date_obs is written with
// time.RFC3339Nano, which DROPS the fractional part at zero nanoseconds while
// the header parser admits fractional seconds, so the column holds both
// "…T16:00:00Z" and "…T16:00:00.031444Z". Since "." (0x2E) sorts below "Z"
// (0x5A), a bound rendered with the zone suffix would place every fractional
// timestamp in the first second of a window BELOW it, sweeping those frames into
// the previous observing date. Truncating to seconds with no suffix brackets
// both spellings, because either is a strict extension of the bound string.
const dateObsBoundLayout = "2006-01-02T15:04:05"

// nightHint is what reading a frame's destination folder yields: the folder
// itself, and the two ordinals AssignNight validates before using.
type nightHint struct {
	// Prefix is the rendered destination above the night segment, relative to
	// the light root.
	Prefix string
	// NightName renders the night folder's own name for a candidate ordinal.
	NightName func(n int) (string, error)
	// OK is false when there is no folder to consult — no <n> segment, or a
	// prefix that renders empty. The hints are then both zero and filing behaves
	// as it does without any consultation.
	OK bool
	// Preferred is the ordinal that folder already holds for this observing date
	// (0 for none); FolderHigh is the highest ordinal it holds at all.
	Preferred, FolderHigh int
}

// folderRange returns the half-open dest_path range covering everything filed at
// or below base. The lower bound carries a trailing separator so a sibling
// directory sharing base's name as a prefix ("M31_old" beside "M31") cannot fall
// inside it, and the guard against a doubled separator matters because a managed
// root may itself be a drive or UNC root that already ends in one. The upper
// bound is the successor of that byte, which excludes true siblings: "_" (0x5F)
// sorts above "0" (0x30), the successor of "/".
func folderRange(base string) (lo, hi string) {
	sep := string(filepath.Separator)
	lo = base
	if !strings.HasSuffix(lo, sep) {
		lo += sep
	}
	return lo, lo[:len(lo)-1] + string(lo[len(lo)-1]+1)
}

// observingWindow returns the half-open [lo, hi) date_obs bounds of one
// observing date, as the exact inverse of route.ObservingDate: that function
// applies an ABSOLUTE -12h and then truncates to the local calendar date, so
// both bounds are rebuilt the same way rather than as a 24-hour span from the
// lower one. A local calendar day is 23 or 25 hours long across a DST
// transition, so an absolute span is wrong at one end — it pulls the next date's
// first hour in, or drops an hour of this date's own frames, either of which
// skews the frame counts an adoption is decided on.
func observingWindow(obs time.Time) (lo, hi string) {
	loT := obs.Add(12 * time.Hour)
	hiT := obs.AddDate(0, 0, 1).Add(12 * time.Hour)
	return loT.UTC().Format(dateObsBoundLayout), hiT.UTC().Format(dateObsBoundLayout)
}

// routeOptionsFor builds the rendering options for one frame's observing date.
// Every filing path calls it once and passes the result to both nightFor and
// route.Route, because if the two ever diverge the folder consulted for a night
// ordinal is not the folder the frame is filed into. Night is left zero: it is
// only known after nightFor returns.
func routeOptionsFor(cfg *model.Config, obs time.Time) route.Options {
	return route.Options{
		Templates:         cfg.Templates,
		ObservingDate:     obs,
		DateFormat:        cfg.DateFormat,
		StripTargetSpaces: cfg.StripTargetSpaces,
		MosaicDetection:   cfg.MosaicDetection,
	}
}

// nightHints resolves a frame's destination night folder and reports what
// AssignNight needs to know about it, plus the folder itself so a caller need
// not re-render it. It never writes.
//
// cfg is a parameter rather than a fresh w.cfg.Load(): the range base is
// joinLocal(cfg.Roots.Light, prefix) and route.Options carries no roots, so
// reading the config here would consult a different snapshot from the one the
// caller hands to route.Route moments later.
//
// The two steps are index-ranged so the observing date and the path provably
// come from the same folder. Candidate ordinals come from the whole prefix
// subtree, which under the shipped mosaic template is an ancestor of every
// panel tree; each candidate is then asked as its own exact folder, so one
// sourced from a sibling subtree returns no rows and drops out. That exact range
// IS the shape check, which is why there is no depth rule: rows nested below the
// night folder still count, and rows outside it cannot.
func (w *Worker) nightHints(ctx context.Context, cfg *model.Config, profile *model.RigProfile,
	cls model.Classification, opts route.Options) (nightHint, error) {
	prefix, nightName, ok, err := route.NightFolder(cls, *profile, opts)
	if err != nil {
		return nightHint{}, err
	}
	if !ok {
		return nightHint{}, nil
	}

	h := nightHint{Prefix: prefix, NightName: nightName, OK: true}
	base := joinLocal(cfg.Roots.Light, prefix)
	lo, hi := folderRange(base)
	nights, err := w.store.NightsUnder(ctx, lo, hi)
	if err != nil {
		return nightHint{}, fmt.Errorf("read night folders under %s: %w", base, err)
	}
	dateLo, dateHi := observingWindow(opts.ObservingDate)

	best := 0
	for _, k := range nights {
		name, nerr := nightName(k)
		if nerr != nil {
			// This candidate's own segment cannot be rendered, so there is no folder
			// to ask about it. Drop the candidate; the frame is unaffected.
			continue
		}
		flo, fhi := folderRange(joinLocal(base, name))
		total, matching, cerr := w.store.NightFolderCounts(ctx, flo, fhi, dateLo, dateHi)
		if cerr != nil {
			return nightHint{}, fmt.Errorf("count night folder %s: %w", name, cerr)
		}
		if total == 0 {
			continue
		}
		if k > h.FolderHigh {
			h.FolderHigh = k
		}
		// The folder that predominantly holds this date is the one the operator
		// means. A count rather than bare highest-wins, because one folder can hold
		// one date at two ordinals — the reported archive's own shape — and
		// highest-wins cements the error whenever the contamination runs upward.
		if matching > 0 && (matching > best || (matching == best && k > h.Preferred)) {
			h.Preferred, best = k, matching
		}
	}
	return h, nil
}

// nightFor resolves the night ordinal a light frame files under: the ledger's
// own answer when this observing date already has one, otherwise an assignment
// informed by what the destination folder holds.
//
// It takes cls by pointer because it settles the frame's target spelling as well
// as its ordinal, and the two cannot be settled apart: <TARGET> renders the
// folder the ordinal is scoped to, so a caller that routed with one spelling
// after scoping with another would consult a folder the frame is not filed into.
// A case-variant OBJECT is folded onto the spelling this target was first filed
// under — see store.CanonicalTarget for why case is target identity here.
//
// Its error is either a *route.QuarantineError — the destination could not be
// rendered, so the caller proceeds with night 0 and lets route.Route re-derive
// the identical quarantine from the same segments — or a store failure, which
// must abort the filing. Filing on with zero hints after a failed read would
// mint scopeMAX+1, which is precisely the land-on-an-occupied-folder failure the
// consultation exists to prevent.
func (w *Worker) nightFor(ctx context.Context, cfg *model.Config, profile *model.RigProfile,
	cls *model.Classification, opts route.Options) (int, error) {
	target, err := w.store.CanonicalTarget(ctx, cls.Target)
	if err != nil {
		return 0, err
	}
	// Every frame of a case-flipped run folds, so this is per-frame and belongs at
	// debug; the assignment log below carries the same fact once per night.
	foldedFrom := ""
	if target != cls.Target {
		foldedFrom = cls.Target
		w.log.Debug("rig: folded target spelling", "from", cls.Target, "to", target)
		cls.Target = target
	}
	key := route.ObservingDateKey(opts.ObservingDate)
	n, ok, err := w.store.LookupNight(ctx, cls.Target, profile.OTA, key)
	if err != nil {
		return 0, err
	}
	if ok {
		return n, nil
	}

	h, err := w.nightHints(ctx, cfg, profile, *cls, opts)
	if err != nil {
		return 0, err
	}
	night, err := w.store.AssignNight(ctx, store.NightRequest{
		Target:          cls.Target,
		OTA:             profile.OTA,
		ObservingDate:   key,
		Preferred:       h.Preferred,
		FolderHighWater: h.FolderHigh,
	})
	if err != nil {
		return 0, err
	}

	// The only field-diagnosable trace of why a frame took a given night: a wrong
	// one is invisible until an operator notices it on disk.
	decision := "mint"
	if h.Preferred >= 1 && h.Preferred == night {
		decision = "adopt"
	}
	folder := "(not consulted)"
	if h.OK {
		folder = h.Prefix
		if name, nerr := h.NightName(night); nerr == nil {
			folder = h.Prefix + "/" + name
		}
	}
	w.log.Info("rig: night assigned", "decision", decision, "night", night,
		"target", cls.Target, "folded_from", foldedFrom, "ota", profile.OTA, "observing_date", key,
		"folder", folder, "folder_holds_date", h.Preferred, "folder_high_water", h.FolderHigh)
	return night, nil
}
