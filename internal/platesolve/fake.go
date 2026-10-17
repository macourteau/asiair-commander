package platesolve

import "context"

// FakeSolver returns scripted results keyed by request path, for worker/store tests
// (mirrors header.Fake). No real binary or network is involved, so CI never touches
// astap_cli. A path present in Errs returns an operational error (transient); a path
// present in Results returns that Result; an unmapped path returns a clean
// no-solution (terminal failed).
type FakeSolver struct {
	Results map[string]Result
	Errs    map[string]error
	// Panics, when a path is present, makes Solve panic — to exercise the worker's
	// per-frame recover + attempt budget.
	Panics map[string]bool
	// Calls records every solved path in order, for assertions.
	Calls []string
}

// Solve returns the scripted outcome for req.Path.
func (f *FakeSolver) Solve(_ context.Context, req Request) (Result, error) {
	f.Calls = append(f.Calls, req.Path)
	if f.Panics[req.Path] {
		panic("platesolve: scripted panic for " + req.Path)
	}
	if err, ok := f.Errs[req.Path]; ok {
		return Result{}, err
	}
	if r, ok := f.Results[req.Path]; ok {
		return r, nil
	}
	return Result{Reason: "no star match"}, nil
}

// FakeGate is a scriptable SolveGate for worker tests: it reports readiness and a
// catalog dir without a real provisioner or disk state.
type FakeGate struct {
	ReadyVal   bool
	CatalogDir string
	CatalogOK  bool
	// FOVFloor, when > 0, makes CatalogFor return ok only for fov ≥ FOVFloor, so a
	// below-floor frame can be exercised.
	FOVFloor float64
	// Pending scripts DownloadPending (a download is queued/running).
	Pending  bool
	Acquired int
	Released int
}

func (g *FakeGate) Ready() bool { return g.ReadyVal }

func (g *FakeGate) CatalogFor(fovDeg float64) (string, bool) {
	if g.FOVFloor > 0 && fovDeg < g.FOVFloor {
		return "", false
	}
	if !g.CatalogOK {
		return "", false
	}
	return g.CatalogDir, true
}

func (g *FakeGate) BeginSolve(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.Acquired++
	return func() { g.Released++ }, nil
}

func (g *FakeGate) DownloadPending() bool { return g.Pending }
