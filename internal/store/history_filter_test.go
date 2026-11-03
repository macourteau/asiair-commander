package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// seedFilterHistory stores a small, varied transfer history and returns the
// store. Each frame's SourcePath is its name in the expectations below.
func seedFilterHistory(t *testing.T) Store {
	t.Helper()
	s := newStore(t)
	frames := []model.Frame{
		{RigID: "rig-a", SourcePath: "a-light-L", FrameType: "LIGHT", Target: "NGC 1499", Filter: "L", Exptime: 600, DateObs: "2026-11-02T03:00:00.5Z", DestPath: "Astro/NGC 1499/rig-a/L/a.fit"},
		{RigID: "rig-a", SourcePath: "a-light-none", FrameType: "LIGHT", Target: "NGC 1499", Exptime: 1.32500004768372, DateObs: "2026-11-03T03:00:00Z", DestPath: "Astro/NGC 1499/rig-a/b.fit"},
		{RigID: "rig-b", SourcePath: "b-light-Ha", FrameType: "LIGHT", Target: "ngc 1499", Filter: "Ha", Exptime: 300, DateObs: "2026-11-02T23:59:59.999999Z", DestPath: "Astro/NGC 1499/rig-b/Ha/c.fit"},
		{RigID: "rig-b", SourcePath: "b-flat-Ha", FrameType: "FLAT", Filter: "Ha", Exptime: 0.5, DateObs: "2026-11-04T10:00:00Z", DestPath: "Astro/Flats/d_100%.fit"},
		{RigID: "rig-c", SourcePath: "c-unknown", Target: "M 31", DestPath: "Astro/M 31/e.fit"},
	}
	for i := range frames {
		f := frames[i]
		f.SourceShare, f.Size, f.Mtime, f.State, f.PulledAt = "s", 1, 1, model.StateVerified, int64(1000+i)
		if err := s.UpsertFrame(context.Background(), &f); err != nil {
			t.Fatal(err)
		}
	}
	// Never pulled: outside the history whatever the filter.
	never := model.Frame{RigID: "rig-a", SourceShare: "s", SourcePath: "never", Size: 1, Mtime: 1, State: model.StateFailed, Filter: "OIII"}
	if err := s.UpsertFrame(context.Background(), &never); err != nil {
		t.Fatal(err)
	}
	return s
}

func unix(t *testing.T, rfc string) int64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		t.Fatal(err)
	}
	return ts.Unix()
}

func TestListTransferHistory_Filters(t *testing.T) {
	s := seedFilterHistory(t)
	ctx := context.Background()
	nov2, nov3 := unix(t, "2026-11-02T00:00:00Z"), unix(t, "2026-11-03T00:00:00Z")

	cases := []struct {
		name string
		f    HistoryFilter
		want []string
	}{
		{"none", HistoryFilter{}, []string{"a-light-L", "a-light-none", "b-light-Ha", "b-flat-Ha", "c-unknown"}},
		{"device in", HistoryFilter{RigIDs: []string{"rig-a", "rig-c"}}, []string{"a-light-L", "a-light-none", "c-unknown"}},
		{"device not in", HistoryFilter{RigIDs: []string{"rig-a"}, RigNot: true}, []string{"b-light-Ha", "b-flat-Ha", "c-unknown"}},
		{"target substring ignores case", HistoryFilter{Target: "NGC 14"}, []string{"a-light-L", "a-light-none", "b-light-Ha"}},
		{"path substring", HistoryFilter{Path: "/rig-b/"}, []string{"b-light-Ha"}},
		{"path wildcard characters are literal", HistoryFilter{Path: "_100%"}, []string{"b-flat-Ha"}},
		{"path % matches only a literal %", HistoryFilter{Path: "%"}, []string{"b-flat-Ha"}},
		{"filter is", HistoryFilter{Filters: []string{"Ha"}}, []string{"b-light-Ha", "b-flat-Ha"}},
		{"filter is none", HistoryFilter{FilterNone: true}, []string{"a-light-none", "c-unknown"}},
		{"filter is L or none", HistoryFilter{Filters: []string{"L"}, FilterNone: true}, []string{"a-light-L", "a-light-none", "c-unknown"}},
		{"filter is not Ha keeps frames without one", HistoryFilter{Filters: []string{"Ha"}, FilterNot: true}, []string{"a-light-L", "a-light-none", "c-unknown"}},
		{"filter is not none", HistoryFilter{FilterNone: true, FilterNot: true}, []string{"a-light-L", "b-light-Ha", "b-flat-Ha"}},
		{"type is", HistoryFilter{FrameTypes: []string{"FLAT"}}, []string{"b-flat-Ha"}},
		{"type is not keeps unknown", HistoryFilter{FrameTypes: []string{"LIGHT"}, TypeNot: true}, []string{"b-flat-Ha", "c-unknown"}},
		{"date in [nov2, nov3)", HistoryFilter{ObsFrom: nov2, ObsTo: nov3}, []string{"a-light-L", "b-light-Ha"}},
		{"date from only", HistoryFilter{ObsFrom: nov3}, []string{"a-light-none", "b-flat-Ha"}},
		{"date before only", HistoryFilter{ObsTo: nov3}, []string{"a-light-L", "b-light-Ha"}},
		{"date not in keeps undated", HistoryFilter{ObsFrom: nov2, ObsTo: nov3, ObsNot: true}, []string{"a-light-none", "b-flat-Ha", "c-unknown"}},
		{"exposure lt", HistoryFilter{Exposure: &ExposureFilter{Op: ExposureLT, Min: 300}}, []string{"a-light-none", "b-flat-Ha"}},
		{"exposure le rounds to ms", HistoryFilter{Exposure: &ExposureFilter{Op: ExposureLE, Min: 1.325}}, []string{"a-light-none", "b-flat-Ha"}},
		{"exposure gt", HistoryFilter{Exposure: &ExposureFilter{Op: ExposureGT, Min: 300}}, []string{"a-light-L"}},
		{"exposure ge", HistoryFilter{Exposure: &ExposureFilter{Op: ExposureGE, Min: 300}}, []string{"a-light-L", "b-light-Ha"}},
		{"exposure in range inclusive", HistoryFilter{Exposure: &ExposureFilter{Op: ExposureIn, Min: 1.325, Max: 300}}, []string{"a-light-none", "b-light-Ha"}},
		{"exposure not in range keeps unrecorded", HistoryFilter{Exposure: &ExposureFilter{Op: ExposureNotIn, Min: 1.325, Max: 300}}, []string{"a-light-L", "b-flat-Ha", "c-unknown"}},
		{"filters combine with AND", HistoryFilter{RigIDs: []string{"rig-b"}, Target: "1499", Filters: []string{"Ha"}}, []string{"b-light-Ha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, total, err := s.ListTransferHistory(ctx, tc.f, 0, 50)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, f := range got {
				names = append(names, f.SourcePath)
			}
			want := slices.Clone(tc.want)
			slices.Sort(names)
			slices.Sort(want)
			if !slices.Equal(names, want) || total != len(want) {
				t.Errorf("got %v (total %d), want %v", names, total, want)
			}
		})
	}
}

func TestHistoryFilter_ValidateRefusesWhatTheQueryCannotExpress(t *testing.T) {
	bad := []HistoryFilter{
		{ObsFrom: 200, ObsTo: 100},
		{ObsFrom: 100, ObsTo: 100},
		{ObsFrom: -1},
		{Exposure: &ExposureFilter{Op: "eq", Min: 1}},
		{Exposure: &ExposureFilter{Op: ExposureLT, Min: -1}},
		{Exposure: &ExposureFilter{Op: ExposureIn, Min: 10, Max: 5}},
	}
	s := newStore(t)
	for _, f := range bad {
		if f.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want an error", f)
		}
		if _, _, err := s.ListTransferHistory(context.Background(), f, 0, 50); err == nil {
			t.Errorf("ListTransferHistory(%+v) ran an invalid filter", f)
		}
	}
	if err := (HistoryFilter{Exposure: &ExposureFilter{Op: ExposureIn, Min: 5, Max: 5}}).Validate(); err != nil {
		t.Errorf("a one-point exposure range is valid: %v", err)
	}
}

func TestTransferHistoryFacets(t *testing.T) {
	s := seedFilterHistory(t)
	fc, err := s.TransferHistoryFacets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// OIII is only on a never-pulled frame, so it is not offered.
	if want := []string{"Ha", "L"}; !slices.Equal(fc.Filters, want) {
		t.Errorf("filters = %v, want %v", fc.Filters, want)
	}
	if want := []string{"FLAT", "LIGHT"}; !slices.Equal(fc.FrameTypes, want) {
		t.Errorf("frame types = %v, want %v", fc.FrameTypes, want)
	}

	empty, err := newStore(t).TransferHistoryFacets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if empty.Filters == nil || empty.FrameTypes == nil {
		t.Errorf("an empty history must give empty lists, not null: %+v", empty)
	}
}
