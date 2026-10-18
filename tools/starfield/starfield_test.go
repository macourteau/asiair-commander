package starfield

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/header"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTangentAtCentreAndAxes(t *testing.T) {
	xi, eta, ok := Tangent(56.75, 24.12, 56.75, 24.12)
	if !ok || xi != 0 || eta != 0 {
		t.Fatalf("centre: got (%g, %g, %v), want (0, 0, true)", xi, eta, ok)
	}
	// One degree due north is tan(1°) on the tangent plane, straight up.
	xi, eta, _ = Tangent(56.75, 25.12, 56.75, 24.12)
	if want := math.Tan(deg) / deg; math.Abs(xi) > 1e-12 || math.Abs(eta-want) > 1e-9 {
		t.Errorf("north: got (%g, %g), want (0, %g)", xi, eta, want)
	}
	// Increasing RA is east, which is +xi.
	if xi, _, _ = Tangent(57.75, 24.12, 56.75, 24.12); xi <= 0 {
		t.Errorf("east: xi = %g, want > 0", xi)
	}
	// The far hemisphere has no gnomonic projection.
	if _, _, ok = Tangent(236.75, -24.12, 56.75, 24.12); ok {
		t.Error("antipode projected; want ok=false")
	}
}

func TestPixelOrientation(t *testing.T) {
	const s = 0.001 // deg/px
	// North up, east left, centre on the frame centre.
	if px, py := Pixel(0, 0, s, 0, 100, 80); px != 50 || py != 40 {
		t.Errorf("centre at (%g, %g), want (50, 40)", px, py)
	}
	if px, py := Pixel(0, 0.01, s, 0, 100, 80); math.Abs(px-50) > 1e-9 || math.Abs(py-30) > 1e-9 {
		t.Errorf("north at (%g, %g), want (50, 30)", px, py)
	}
	if px, _ := Pixel(0.01, 0, s, 0, 100, 80); math.Abs(px-40) > 1e-9 {
		t.Errorf("east at x=%g, want 40 (left of centre)", px)
	}
	// Half a turn puts north at the bottom.
	if _, py := Pixel(0, 0.01, s, 180, 100, 80); math.Abs(py-50) > 1e-9 {
		t.Errorf("north at 180° rotation: y=%g, want 50", py)
	}
}

func TestFluxIsMagnitudeScale(t *testing.T) {
	if r := Flux(5, 60, 120) / Flux(10, 60, 120); math.Abs(r-100) > 1e-9 {
		t.Errorf("5 magnitudes = %gx flux, want 100x", r)
	}
	if r := Flux(10, 120, 60) / Flux(10, 60, 60); math.Abs(r-4) > 1e-9 {
		t.Errorf("double aperture = %gx flux, want 4x", r)
	}
	if r := Flux(10, 60, 240) / Flux(10, 60, 60); math.Abs(r-4) > 1e-9 {
		t.Errorf("4x exposure = %gx flux, want 4x", r)
	}
}

// smallTarget is a frame small enough to render in a test, framed like the
// presets.
func smallTarget() Target {
	cam := ASI533MM
	cam.Width, cam.Height = 200, 150
	return Target{Name: "Test Field", RA: 56.75, Dec: 24.12, Optics: Optics{FocalLengthMm: 300, ApertureMm: 60, Camera: cam, Bin: 1},
		Exposure: 120, Filter: "L", MagLimit: 14}
}

// A rendered star's centroid is where the projection puts it: the property a plate
// solver relies on.
func TestRenderPutsStarWhereProjected(t *testing.T) {
	tg := smallTarget()
	// A star 0.02° north-west of centre.
	star := Star{RA: tg.RA - 0.03, Dec: tg.Dec + 0.02, G: 10}
	img := Render(tg, []Star{star}, 1)
	xi, eta, _ := Tangent(star.RA, star.Dec, tg.RA, tg.Dec)
	wantX, wantY := Pixel(xi, eta, tg.Optics.ScaleDeg(), 0, img.Width, img.Height)

	var sum, sx, sy float64
	for y := int(wantY) - 6; y <= int(wantY)+6; y++ {
		for x := int(wantX) - 6; x <= int(wantX)+6; x++ {
			v := float64(img.Pix[y*img.Width+x]) - 4000 // well above the sky, below the core
			if v > 0 {
				sum += v
				sx += v * (float64(x) + 0.5)
				sy += v * (float64(y) + 0.5)
			}
		}
	}
	if sum == 0 {
		t.Fatal("no star signal near the projected position")
	}
	if dx, dy := sx/sum-wantX, sy/sum-wantY; math.Hypot(dx, dy) > 0.25 {
		t.Errorf("centroid off by (%.2f, %.2f) px from the projection at (%.1f, %.1f)", dx, dy, wantX, wantY)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	tg := smallTarget()
	stars := []Star{{RA: tg.RA, Dec: tg.Dec, G: 9}}
	a, b := Render(tg, stars, 7), Render(tg, stars, 7)
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			t.Fatalf("pixel %d differs between identical renders", i)
		}
	}
	if c := Render(tg, stars, 8); c.Pix[0] == a.Pix[0] && c.Pix[1] == a.Pix[1] && c.Pix[2] == a.Pix[2] {
		t.Error("a different seed gave the same noise")
	}
}

func TestDataUnitRoundTrips(t *testing.T) {
	img := Image{Width: 3, Height: 2, Pix: []uint16{0, 1, 32767, 32768, 65534, 65535}}
	data := DataUnit(img)
	if len(data)%2880 != 0 {
		t.Fatalf("data unit is %d bytes, not whole blocks", len(data))
	}
	back, err := Decode(data, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range img.Pix {
		if back.Pix[i] != img.Pix[i] {
			t.Errorf("pixel %d: %d round-tripped to %d", i, img.Pix[i], back.Pix[i])
		}
	}
}

// The header is what Commander classifies and plate-solves from: it parses with the
// real reader, carries the hints a solve needs, no embedded solution, and nothing
// on the privacy denylist.
func TestHeaderIsASIAIRShapedAndCarriesNoPII(t *testing.T) {
	tg := smallTarget()
	e := Exposure{Start: time.Date(2026, 10, 7, 23, 15, 2, 500000000, time.UTC), SetTemp: -10, CCDTemp: -9.8, Seq: 3}
	path := filepath.Join(t.TempDir(), "f.fit")
	if err := os.WriteFile(path, File(Cards(tg, e), DataUnit(Render(tg, nil, 1))), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := header.New().ReadHeader(path)
	if err != nil {
		t.Fatalf("real header reader refused the frame: %v", err)
	}
	for key, want := range map[string]any{
		"IMAGETYP": "Light", "OBJECT": "Test Field", "FILTER": "L", "INSTRUME": "ZWO ASI533MM Pro",
		"DATE-OBS": "2026-10-07T23:15:02.500000",
	} {
		if got, _ := h.Str(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]float64{
		"NAXIS1": 200, "NAXIS2": 150, "EXPTIME": 120, "GAIN": 100, "SET-TEMP": -10, "XBINNING": 1, "FOCALLEN": 300, "XPIXSZ": 3.76,
	} {
		if got, ok := h.Float(key); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", key, got, ok, want)
		}
	}
	a := astrometry.FromHeader(h)
	if a.Solution != nil {
		t.Error("frame carries an embedded WCS; it should leave the solve to Commander")
	}
	if a.Hints == nil || a.Hints.RA == nil || a.Hints.Dec == nil || a.Hints.FocalLenMM == nil || a.Hints.PixelSizeUm == nil {
		t.Fatalf("hints incomplete: %+v", a.Hints)
	}
	if d := math.Hypot((*a.Hints.RA-tg.RA)*math.Cos(tg.Dec*deg), *a.Hints.Dec-tg.Dec); d < 0.01 || d > 0.2 {
		t.Errorf("pointing hint is %.3f° from the field centre, want a goto-sized miss", d)
	}
	for _, k := range h.Keys() {
		if strings.HasPrefix(k, "GPS") {
			t.Errorf("header carries %s", k)
		}
	}
	for _, banned := range header.PIIDenylist {
		if _, ok := h.Card(banned); ok {
			t.Errorf("header carries privacy-denylisted %s", banned)
		}
	}
}

func TestFrameNameIsASIAIRShaped(t *testing.T) {
	tg := smallTarget()
	e := Exposure{Start: time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC), CCDTemp: -10, Seq: 12}
	want := "Light_Test Field_120.0s_Bin1_533MM_gain100_20261007-231502_-10.0C_0012.fit"
	if got := FrameName(tg, e); got != want {
		t.Errorf("FrameName = %q, want %q", got, want)
	}
}

// tapStub serves the archive's CSV for any query and counts requests.
func tapStub(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var calls atomic.Int32
	var lastQuery atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		lastQuery.Store(v.Get("QUERY"))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &lastQuery
}

const stubCSV = "ra,dec,phot_g_mean_mag\n56.75,24.12,9.5\n56.80,24.10,12.25\n56.70,24.15,\n"

func TestConeFetchesOnceThenServesFromCache(t *testing.T) {
	srv, calls, q := tapStub(t, http.StatusOK, stubCSV)
	g := &Gaia{Dir: t.TempDir(), TAPURL: srv.URL, Log: quietLog()}
	ctx := context.Background()

	stars, err := g.Cone(ctx, 56.75, 24.12, 1.5, 14.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(stars) != 2 || stars[1] != (Star{RA: 56.80, Dec: 24.10, G: 12.25}) {
		t.Fatalf("stars = %+v; want two (the row with no magnitude skipped)", stars)
	}
	query := q.Load().(string)
	for _, want := range []string{"gaiadr3.gaia_source_lite", "phot_g_mean_mag <= 14.5", "CIRCLE('ICRS', 56.750000, 24.120000, 1.500000)"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %q", query, want)
		}
	}

	again, err := g.Cone(ctx, 56.75, 24.12, 1.5, 14.5)
	if err != nil || len(again) != 2 {
		t.Fatalf("second Cone = %d stars, %v", len(again), err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("archive queried %d times; the second Cone should hit the cache", n)
	}
	// A different magnitude limit is a different cone.
	if _, err := g.Cone(ctx, 56.75, 24.12, 1.5, 15); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("archive queried %d times; a new magnitude limit should miss the cache", n)
	}
}

func TestConeFailureIsAnErrorAndCachesNothing(t *testing.T) {
	srv, _, _ := tapStub(t, http.StatusServiceUnavailable, "maintenance")
	dir := t.TempDir()
	g := &Gaia{Dir: dir, TAPURL: srv.URL, Log: quietLog()}
	_, err := g.Cone(context.Background(), 56.75, 24.12, 1.5, 14.5)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the HTTP status", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed fetch left %d cache entries", len(entries))
	}

	unreachable := &Gaia{Dir: dir, TAPURL: "http://127.0.0.1:1/tap/sync", Log: quietLog()}
	if _, err := unreachable.Cone(context.Background(), 56.75, 24.12, 1.5, 14.5); err == nil {
		t.Error("an unreachable archive returned no error")
	}
}

func TestConeRefusesATruncatedResult(t *testing.T) {
	old := maxRows
	maxRows = 2
	t.Cleanup(func() { maxRows = old })
	srv, _, q := tapStub(t, http.StatusOK, "ra,dec,phot_g_mean_mag\n1,1,10\n2,2,11\n3,3,12\n")
	g := &Gaia{Dir: t.TempDir(), TAPURL: srv.URL, Log: quietLog()}
	if _, err := g.Cone(context.Background(), 1, 1, 1, 14); err == nil {
		t.Fatal("a result over the row cap was accepted")
	}
	if !strings.Contains(q.Load().(string), "TOP 3 ") {
		t.Errorf("query %q does not ask for one row over the cap", q.Load())
	}
}

func TestConeRefetchesACorruptCacheEntry(t *testing.T) {
	srv, calls, _ := tapStub(t, http.StatusOK, stubCSV)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, coneKey(56.75, 24.12, 1.5, 14.5)+".csv"), []byte("<html>oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &Gaia{Dir: dir, TAPURL: srv.URL, Log: quietLog()}
	if stars, err := g.Cone(context.Background(), 56.75, 24.12, 1.5, 14.5); err != nil || len(stars) != 2 {
		t.Fatalf("Cone = %d stars, %v", len(stars), err)
	}
	if calls.Load() != 1 {
		t.Error("a corrupt cache entry was trusted instead of re-fetched")
	}
}

// A render is cached: once made, it is served with the archive unreachable, and
// re-heading it per exposure gives distinct files over identical pixels.
func TestSkyCachesRendersAndReheadsPerExposure(t *testing.T) {
	srv, calls, _ := tapStub(t, http.StatusOK, stubCSV)
	dir := t.TempDir()
	tg := smallTarget()
	sky := &Sky{Dir: dir, Gaia: &Gaia{Dir: filepath.Join(dir, "gaia"), TAPURL: srv.URL, Log: quietLog()}, Log: quietLog()}
	first, err := sky.Render(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}

	offline := &Sky{Dir: dir, Gaia: &Gaia{Dir: filepath.Join(dir, "gaia"), TAPURL: "http://127.0.0.1:1/", Log: quietLog()}, Log: quietLog()}
	second, err := offline.Render(context.Background(), tg)
	if err != nil {
		t.Fatalf("cached render not served offline: %v", err)
	}
	if string(second.File) != string(first.File) || calls.Load() != 1 {
		t.Error("the second render was not the cached one")
	}

	a := first.FITS(Exposure{Start: time.Unix(1_800_000_000, 0), Seq: 1})
	b := first.FITS(Exposure{Start: time.Unix(1_800_000_130, 0), Seq: 2})
	if string(a) == string(b) {
		t.Error("two exposures produced identical files")
	}
	da, _ := dataUnit(a)
	db, _ := dataUnit(b)
	if string(da) != string(db) || string(da) != string(first.Data) {
		t.Error("re-heading changed the pixels")
	}
	if want := tg.Optics.Width() * tg.Optics.Height() * 2; len(first.Data) < want {
		t.Errorf("data unit %d bytes, want at least %d", len(first.Data), want)
	}
}

func TestPresetsFrameTheirTargets(t *testing.T) {
	seen := map[string]bool{}
	for _, tg := range Targets {
		if seen[tg.Name] {
			t.Errorf("duplicate preset %q", tg.Name)
		}
		seen[tg.Name] = true
		_, h := tg.Optics.FOV()
		// ASTAP's default d05 catalog solves fields of 0.6° and up.
		if h < 0.6 {
			t.Errorf("%s: field height %.2f° is below the d05 catalog's floor", tg.Name, h)
		}
		if got, ok := TargetByName(tg.Name); !ok || got.Name != tg.Name {
			t.Errorf("TargetByName(%q) = %v, %v", tg.Name, got.Name, ok)
		}
		if strings.ContainsAny(tg.Name, "/_") {
			t.Errorf("%s: a name with / or _ would break the ASIAIR path or frame name", tg.Name)
		}
	}
	if _, ok := TargetByName("nope"); ok {
		t.Error("TargetByName found a target that is not a preset")
	}
}
