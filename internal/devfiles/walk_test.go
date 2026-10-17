package devfiles

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/macourteau/asiair-commander/internal/device"
)

// seedTree stages Preview/{a/{1.fit,2.fit}, b/{3.fit}, c.jpg} on r1's eMMC.
func seedTree(fx *fixture) *device.Fake {
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a/1.fit", []byte("11"), t0)
	f.AddFile(emmc, "Preview/a/2.fit", []byte("222"), t0)
	f.AddFile(emmc, "Preview/b/3.fit", []byte("3"), t0)
	f.AddFile(emmc, "Preview/c.jpg", []byte("cccc"), t0)
	return f
}

func rels(es []entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Rel
		if e.Dir {
			out[i] += "/"
		}
	}
	return out
}

func TestWalk(t *testing.T) {
	fx := newFixture(t)
	seedTree(fx)
	es, err := fx.svc.walk(context.Background(), "Delete check", "r1", emmc, "Preview", []string{"a", "c.jpg", "b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/", "a/1.fit", "a/2.fit", "c.jpg", "b/", "b/3.fit"}
	if got := rels(es); !reflect.DeepEqual(got, want) {
		t.Errorf("walk = %v, want %v (pre-order, selection order)", got, want)
	}
	if es[2].Size != 3 || !es[2].Mtime.Equal(t0) {
		t.Errorf("a/2.fit = %+v, want size 3 at t0", es[2])
	}
}

func TestWalkFailsWhole(t *testing.T) {
	ctx := context.Background()
	failOn := func(f *device.Fake, dir string, n int, err error) {
		f.OnListDir = func(_, d string) {
			if d == dir {
				f.FailNext(device.FakeOpListDir, n, err)
			}
		}
	}

	t.Run("a refused listing fails the walk", func(t *testing.T) {
		fx := newFixture(t)
		failOn(seedTree(fx), "Preview/b", 1, &os.PathError{Op: "listdir", Err: os.ErrPermission})
		es, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", []string{"a", "b", "c.jpg"})
		wantKind(t, err, ErrDenied)
		if es != nil {
			t.Errorf("entries = %v, want none", rels(es))
		}
	})

	t.Run("a folder gone mid-walk fails the walk", func(t *testing.T) {
		fx := newFixture(t)
		failOn(seedTree(fx), "Preview/a", 1, device.NotFoundError("listdir Preview/a"))
		es, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", []string{"a", "b"})
		wantKind(t, err, ErrNotFound)
		if es != nil {
			t.Errorf("entries = %v, want none", rels(es))
		}
	})

	t.Run("a listing that never answers fails the walk", func(t *testing.T) {
		fx := newFixture(t)
		f := seedTree(fx)
		f.OnListDir = func(_, d string) {
			if d == "Preview/b" {
				f.FailNext(device.FakeOpListDir, 1, errNetworkForTest())
			}
		}
		es, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", []string{"a", "b"})
		wantKind(t, err, ErrUnreachable)
		if es != nil {
			t.Errorf("entries = %v, want none", rels(es))
		}
	})

	t.Run("a listing that recovers within its retries completes", func(t *testing.T) {
		fx := newFixture(t)
		f := seedTree(fx)
		failures := 2
		f.OnListDir = func(_, d string) {
			if d == "Preview/b" && failures > 0 {
				failures--
				f.FailNext(device.FakeOpListDir, 1, errNetworkForTest())
			}
		}
		es, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", []string{"a", "b"})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(es); got != 5 {
			t.Errorf("entries = %v, want all 5", rels(es))
		}
	})

	t.Run("a selected name that is gone fails the walk", func(t *testing.T) {
		fx := newFixture(t)
		seedTree(fx)
		_, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", []string{"a", "zz"})
		wantKind(t, err, ErrNotFound)
	})

	t.Run("more files than the cap is too_many", func(t *testing.T) {
		fx := newFixture(t)
		seedTree(fx)
		fx.svc.maxFiles = 4
		if _, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", []string{"a", "b", "c.jpg"}); err != nil {
			t.Fatalf("exactly at the cap: %v", err)
		}
		fx.svc.maxFiles = 3
		es, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", []string{"a", "b", "c.jpg"})
		wantKind(t, err, ErrTooMany)
		if es != nil {
			t.Errorf("entries = %v, want none", rels(es))
		}
	})

	t.Run("a cancel between listings stops the walk", func(t *testing.T) {
		fx := newFixture(t)
		f := seedTree(fx)
		cctx, cancel := context.WithCancel(ctx)
		f.OnListDir = func(_, d string) {
			if d == "Preview/a" {
				cancel()
			}
		}
		_, err := fx.svc.walk(cctx, "Delete check", "r1", emmc, "Preview", []string{"a", "b"})
		wantKind(t, err, ErrCancelled)
	})

	t.Run("bad selections are refused before any listing", func(t *testing.T) {
		fx := newFixture(t)
		seedTree(fx)
		for _, names := range [][]string{nil, {"a", "a"}, {"../x"}, {"a/1.fit"}, {""}} {
			_, err := fx.svc.walk(ctx, "Delete check", "r1", emmc, "Preview", names)
			wantKind(t, err, ErrBadPath)
		}
		if got := fx.opens.Load(); got != 0 {
			t.Errorf("opens = %d, want 0", got)
		}
	})
}
