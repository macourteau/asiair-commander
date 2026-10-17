package netsite

import (
	"context"
	"errors"
	"testing"
)

// TestLocalSiteMeasuresWithTheLocalRead (local parity): on the local site, as
// the registry hands it out, a digest measurement is the caller's own read,
// run exactly once, with its digests and its error returned unmodified;
// unprobed, since Commander's session already confirmed identity, and
// recordable, since this build computed them.
func TestLocalSiteMeasuresWithTheLocalRead(t *testing.T) {
	site := NewRegistry(nil).Site(Local)
	want := Digests{Whole: "whole-digest", Data: "data-digest"}
	errRead := errors.New("read failed")

	for _, c := range []struct {
		name    string
		digests Digests
		err     error
	}{
		{"digests", want, nil},
		{"error", Digests{}, errRead},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			req := DigestRequest{
				IP: "10.0.0.5", Share: "EMMC Images", Path: "a.fit", User: "u", Password: "p",
				Stop: func() bool { return false },
				Local: func() (Digests, error) {
					calls++
					return c.digests, c.err
				},
			}
			res, err := site.MeasureDigests(context.Background(), req)
			if calls != 1 {
				t.Fatalf("the local read ran %d times, want once", calls)
			}
			if c.err != nil {
				if err != c.err || res != (DigestResult{}) {
					t.Fatalf("result %+v, err %v; want the read's own error unchanged", res, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MeasureDigests: %v", err)
			}
			if res != (DigestResult{Digests: want, DataRecordable: true}) {
				t.Fatalf("result %+v, want the read's digests, unprobed and recordable", res)
			}
		})
	}
}

// TestUnavailableSiteMeasuresNothing: a site that is not up, or an unknown
// one, refuses a measurement as it refuses a dial, without running the read.
func TestUnavailableSiteMeasuresNothing(t *testing.T) {
	reg := NewRegistry(nil)
	down := NewFake("r1")
	down.SetState(StateDown)
	if err := reg.Add(down); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Site{"down": reg.Site("r1"), "unknown": reg.Site("r9")} {
		calls := 0
		_, err := s.MeasureDigests(context.Background(), DigestRequest{
			Stop:  func() bool { return false },
			Local: func() (Digests, error) { calls++; return Digests{}, nil },
		})
		var ue *UnavailableError
		if !errors.As(err, &ue) || calls != 0 {
			t.Fatalf("%s: err %v, %d reads; want an unavailable site and no read", name, err, calls)
		}
	}
}
