package store

import (
	"context"
	"fmt"
	"strings"
)

// HistoryFilter narrows the transfer history (ListTransferHistory) for the
// Tools page's Find frames list (design header-edits §9.4). The zero value
// matches every pulled frame. Each field is one filter; a set field adds one
// predicate and the predicates are ANDed.
//
// Every "Not" flag negates its whole filter, and a frame missing the field
// (NULL or empty) never matches the positive form, so it always matches the
// negated one: "filter is not L" keeps frames without a FILTER card, and
// "exposure not in 60..300" keeps frames with no exposure recorded.
type HistoryFilter struct {
	// RigIDs keeps frames from these rigs (none set: every rig).
	RigIDs []string
	RigNot bool

	// PulledFrom/PulledTo are inclusive unix-second bounds on pulled_at
	// (0 = unbounded on that side).
	PulledFrom, PulledTo int64

	// Target and Path are case-insensitive (ASCII) substrings of the target
	// and of dest_path, the archive path.
	Target string
	Path   string

	// Filters keeps frames whose FILTER is one of these; FilterNone also keeps
	// frames without one. Neither set: no filter predicate.
	Filters    []string
	FilterNone bool
	FilterNot  bool

	// FrameTypes keeps frames of these canonical types (LIGHT, DARK, ...).
	FrameTypes []string
	TypeNot    bool

	// ObsFrom/ObsTo bound DATE-OBS as unix seconds, half-open [ObsFrom, ObsTo);
	// 0 is unbounded on that side. Both 0: no date predicate.
	ObsFrom, ObsTo int64
	ObsNot         bool

	// Exposure compares the exposure, in seconds rounded to the millisecond (as
	// the UI shows it, so "≤ 1.325" holds for a float32-noisy 1.32500004).
	Exposure *ExposureFilter
}

// ExposureOp is the comparison an ExposureFilter applies.
type ExposureOp string

const (
	ExposureLT    ExposureOp = "lt"
	ExposureLE    ExposureOp = "le"
	ExposureGT    ExposureOp = "gt"
	ExposureGE    ExposureOp = "ge"
	ExposureIn    ExposureOp = "in"  // Min <= exposure <= Max
	ExposureNotIn ExposureOp = "out" // the complement of ExposureIn
)

// ExposureFilter is one exposure comparison. The single-bound ops read Min; the
// range ops read Min and Max, both inclusive.
type ExposureFilter struct {
	Op       ExposureOp
	Min, Max float64
}

// Validate reports a filter the query cannot express: an unknown exposure op, a
// negative or inverted bound, or an empty or inverted date range.
func (f HistoryFilter) Validate() error {
	if f.ObsFrom < 0 || f.ObsTo < 0 {
		return fmt.Errorf("date bounds must not be negative")
	}
	if f.ObsFrom > 0 && f.ObsTo > 0 && f.ObsTo <= f.ObsFrom {
		return fmt.Errorf("date range is empty: the end must come after the start")
	}
	if e := f.Exposure; e != nil {
		switch e.Op {
		case ExposureLT, ExposureLE, ExposureGT, ExposureGE:
			if e.Min < 0 {
				return fmt.Errorf("exposure must not be negative")
			}
		case ExposureIn, ExposureNotIn:
			if e.Min < 0 || e.Max < e.Min {
				return fmt.Errorf("exposure range must run from a smaller to a larger value")
			}
		default:
			return fmt.Errorf("unknown exposure comparison %q", e.Op)
		}
	}
	return nil
}

// where renders f as SQL predicates over frames, with their arguments.
func (f HistoryFilter) where() ([]string, []any) {
	var where []string
	var args []any
	// add appends a predicate, negated as NOT COALESCE(p, 0) when not is set so
	// a NULL field (which leaves p NULL) lands in the negated set.
	add := func(not bool, pred string, a ...any) {
		if not {
			pred = "NOT COALESCE((" + pred + "), 0)"
		}
		where = append(where, pred)
		args = append(args, a...)
	}

	if len(f.RigIDs) > 0 {
		add(f.RigNot, "rig_id IN ("+placeholders(len(f.RigIDs))+")", anys(f.RigIDs)...)
	}
	if f.PulledFrom > 0 {
		add(false, "pulled_at >= ?", f.PulledFrom)
	}
	if f.PulledTo > 0 {
		add(false, "pulled_at <= ?", f.PulledTo)
	}
	// instr, not LIKE: the needle is literal, so % and _ in it need no escaping.
	if f.Target != "" {
		add(false, "instr(lower(target), lower(?)) > 0", f.Target)
	}
	if f.Path != "" {
		add(false, "instr(lower(dest_path), lower(?)) > 0", f.Path)
	}
	if len(f.Filters) > 0 || f.FilterNone {
		var ors []string
		if len(f.Filters) > 0 {
			ors = append(ors, "filter IN ("+placeholders(len(f.Filters))+")")
		}
		if f.FilterNone {
			ors = append(ors, "filter IS NULL OR filter = ''")
		}
		add(f.FilterNot, strings.Join(ors, " OR "), anys(f.Filters)...)
	}
	if len(f.FrameTypes) > 0 {
		add(f.TypeNot, "frame_type IN ("+placeholders(len(f.FrameTypes))+")", anys(f.FrameTypes)...)
	}
	if f.ObsFrom > 0 || f.ObsTo > 0 {
		// date_obs is RFC 3339 UTC text (fractional seconds optional), which
		// unixepoch parses; an unparseable or missing value yields NULL.
		var preds []string
		var a []any
		if f.ObsFrom > 0 {
			preds = append(preds, "unixepoch(date_obs) >= ?")
			a = append(a, f.ObsFrom)
		}
		if f.ObsTo > 0 {
			preds = append(preds, "unixepoch(date_obs) < ?")
			a = append(a, f.ObsTo)
		}
		add(f.ObsNot, strings.Join(preds, " AND "), a...)
	}
	if e := f.Exposure; e != nil {
		// 0 is the "not recorded" sentinel (the UI shows "—"), so it counts as
		// missing: it matches no comparison and every negated one.
		const exp = "round(NULLIF(exptime, 0), 3)"
		switch e.Op {
		case ExposureLT:
			add(false, exp+" < ?", e.Min)
		case ExposureLE:
			add(false, exp+" <= ?", e.Min)
		case ExposureGT:
			add(false, exp+" > ?", e.Min)
		case ExposureGE:
			add(false, exp+" >= ?", e.Min)
		case ExposureIn, ExposureNotIn:
			add(e.Op == ExposureNotIn, exp+" BETWEEN ? AND ?", e.Min, e.Max)
		}
	}
	return where, args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// HistoryFacets lists the distinct values the transfer history holds for the
// Find frames list's value pickers, so a picker offers every value on any page
// rather than only those on the loaded one.
type HistoryFacets struct {
	Filters    []string `json:"filters"`
	FrameTypes []string `json:"frame_types"`
}

// TransferHistoryFacets returns the distinct non-empty FILTER and frame-type
// values among pulled frames, sorted.
func (s *sqlStore) TransferHistoryFacets(ctx context.Context) (HistoryFacets, error) {
	distinct := func(col string) ([]string, error) {
		rows, err := s.read.QueryContext(ctx, `SELECT DISTINCT `+col+` FROM frames
			WHERE pulled_at IS NOT NULL AND `+col+` IS NOT NULL AND `+col+` <> '' ORDER BY `+col)
		if err != nil {
			return nil, fmt.Errorf("history facet %s: %w", col, err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, fmt.Errorf("history facet %s: %w", col, err)
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	var fc HistoryFacets
	var err error
	if fc.Filters, err = distinct("filter"); err != nil {
		return HistoryFacets{}, err
	}
	if fc.FrameTypes, err = distinct("frame_type"); err != nil {
		return HistoryFacets{}, err
	}
	return fc, nil
}
