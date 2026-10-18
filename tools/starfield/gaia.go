package starfield

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultTAPURL is the ESA Gaia archive's synchronous TAP endpoint. A cone query
// for one demo field returns a few thousand to a few tens of thousands of rows in
// seconds, well inside what the sync endpoint serves, so the async UWS job dance
// is not needed.
const DefaultTAPURL = "https://gea.esac.esa.int/tap-server/tap/sync"

// maxRows caps a cone query. The query asks for one row more than this, so a
// result that reaches it is known to be truncated and is refused: a field missing
// its fainter half would render, but would not be the sky it claims to be.
var maxRows = 200000

// fetchTimeout bounds one cone query, including the download.
const fetchTimeout = 3 * time.Minute

// Star is one Gaia DR3 source: ICRS position in degrees and G-band magnitude.
type Star struct {
	RA, Dec float64
	G       float64
}

// Gaia fetches magnitude-limited cones from the Gaia archive and caches each
// response verbatim under Dir, keyed by the cone and the magnitude limit, so a
// field is downloaded once per machine. The cache is local data under the Gaia
// licence (CC BY-NC 3.0 IGO) and must never be committed or shipped.
type Gaia struct {
	Dir    string // cache directory; created on first write
	TAPURL string // DefaultTAPURL when empty
	HTTP   *http.Client
	Log    *slog.Logger
}

// Cone returns every star within radiusDeg of (ra, dec) no fainter than magLimit,
// from the cache when present, else from the archive (and then cached).
func (g *Gaia) Cone(ctx context.Context, ra, dec, radiusDeg, magLimit float64) ([]Star, error) {
	log := g.Log
	if log == nil {
		log = slog.Default()
	}
	path := filepath.Join(g.Dir, coneKey(ra, dec, radiusDeg, magLimit)+".csv")
	if b, err := os.ReadFile(path); err == nil {
		stars, perr := parseStars(bytes.NewReader(b))
		if perr == nil {
			log.Info("gaia: cache hit", "file", filepath.Base(path), "stars", len(stars))
			return stars, nil
		}
		// A corrupt cache entry is re-fetched rather than trusted.
		log.Warn("gaia: cached cone unreadable; re-fetching", "file", filepath.Base(path), "error", perr)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("gaia: read cache: %w", err)
	}

	start := time.Now()
	body, err := g.fetch(ctx, coneQuery(ra, dec, radiusDeg, magLimit))
	if err != nil {
		return nil, err
	}
	stars, err := parseStars(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gaia: parse response: %w", err)
	}
	if len(stars) > maxRows {
		return nil, fmt.Errorf("gaia: cone (%.4f, %+.4f) r=%.3f° G<=%.1f exceeds %d rows; narrow the field or the magnitude limit",
			ra, dec, radiusDeg, magLimit, maxRows)
	}
	log.Info("gaia: fetched cone", "file", filepath.Base(path), "stars", len(stars), "elapsed", time.Since(start).Round(time.Millisecond))
	if err := writeAtomic(path, body); err != nil {
		// The stars are good; only the cache write failed, so the next run fetches again.
		log.Warn("gaia: could not cache cone", "file", path, "error", err)
	}
	return stars, nil
}

func (g *Gaia) fetch(ctx context.Context, query string) ([]byte, error) {
	endpoint := g.TAPURL
	if endpoint == "" {
		endpoint = DefaultTAPURL
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	form := url.Values{
		"REQUEST": {"doQuery"},
		"LANG":    {"ADQL"},
		"FORMAT":  {"csv"},
		"QUERY":   {query},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gaia: query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("gaia: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("gaia: query: HTTP %d: %s", resp.StatusCode, snippet)
	}
	return body, nil
}

// coneQuery is the ADQL for one field against gaia_source_lite, the archive's
// narrow copy of gaia_source that carries the columns a renderer needs.
func coneQuery(ra, dec, radiusDeg, magLimit float64) string {
	return fmt.Sprintf("SELECT TOP %d ra, dec, phot_g_mean_mag FROM gaiadr3.gaia_source_lite "+
		"WHERE phot_g_mean_mag <= %.1f "+
		"AND 1 = CONTAINS(POINT('ICRS', ra, dec), CIRCLE('ICRS', %.6f, %.6f, %.6f))",
		maxRows+1, magLimit, ra, dec, radiusDeg)
}

// coneKey names a cached cone. It rounds like coneQuery formats, so two requests
// that send the same query share one entry.
func coneKey(ra, dec, radiusDeg, magLimit float64) string {
	return fmt.Sprintf("gaiadr3_ra%08.4f_dec%+08.4f_r%.3f_g%.1f", ra, dec, radiusDeg, magLimit)
}

// parseStars reads the archive's CSV: a header row naming ra, dec and
// phot_g_mean_mag, then one row per source. A row with an empty magnitude (a
// source with no G photometry) is skipped.
func parseStars(r io.Reader) ([]Star, error) {
	cr := csv.NewReader(r)
	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("csv header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	iRA, okRA := col["ra"]
	iDec, okDec := col["dec"]
	iG, okG := col["phot_g_mean_mag"]
	if !okRA || !okDec || !okG {
		return nil, fmt.Errorf("csv header %q lacks ra, dec or phot_g_mean_mag", strings.Join(head, ","))
	}
	var stars []Star
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return stars, nil
		}
		if err != nil {
			return nil, err
		}
		if rec[iG] == "" {
			continue
		}
		ra, err1 := strconv.ParseFloat(rec[iRA], 64)
		dec, err2 := strconv.ParseFloat(rec[iDec], 64)
		g, err3 := strconv.ParseFloat(rec[iG], 64)
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, fmt.Errorf("csv row %q: %w", strings.Join(rec, ","), err)
		}
		stars = append(stars, Star{RA: ra, Dec: dec, G: g})
	}
}

// writeAtomic writes b to path through a temp file and a rename, so a crash
// never leaves a half-written cache entry that a later run would read.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
