package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// frameDTO is the JSON row shape for a frame, snake_cased to mirror the frames
// DDL and the SSE payloads (design architecture §8) since model.Frame carries
// no JSON tags.
type frameDTO struct {
	ID          int64           `json:"id"`
	RigID       string          `json:"rig_id"`
	SourceShare string          `json:"source_share"`
	SourcePath  string          `json:"source_path"`
	SHA256      string          `json:"sha256,omitempty"`
	Size        int64           `json:"size"`
	Mtime       int64           `json:"mtime"`
	FrameType   string          `json:"frame_type"`
	Target      string          `json:"target,omitempty"`
	Filter      string          `json:"filter,omitempty"`
	Exptime     float64         `json:"exptime"`
	SetTemp     float64         `json:"set_temp"`
	Gain        int64           `json:"gain"`
	Offset      int64           `json:"offset"`
	Binning     string          `json:"binning,omitempty"`
	Sensor      string          `json:"sensor,omitempty"`
	DateObs     string          `json:"date_obs,omitempty"`
	Night       int64           `json:"night"`
	DestPath    string          `json:"dest_path,omitempty"`
	Sidecars    []model.Sidecar `json:"sidecars,omitempty"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	NextRetryAt int64           `json:"next_retry_at,omitempty"`
	PulledAt    int64           `json:"pulled_at,omitempty"`
	DeletedAt   int64           `json:"deleted_at,omitempty"`
	Error       string          `json:"error,omitempty"`
	// MissingSeenAt / RestoreRequestedAt surface out-of-band deletion reconciliation
	// state so the UI can badge a missing frame and its deletion deadline.
	MissingSeenAt      int64 `json:"missing_seen_at,omitempty"`
	RestoreRequestedAt int64 `json:"restore_requested_at,omitempty"`
	// MissingAckedAt is meaningful ONLY alongside a live missing mark (design
	// library-reconciliation §10.5); a reader must test MissingSeenAt != 0 before
	// rendering an Acknowledged pill.
	MissingAckedAt int64 `json:"missing_acked_at,omitempty"`
	// Astrometry reuses model.Astrometry directly (its nested types already carry
	// snake_case JSON tags), so the same payload flows to REST and the live SSE feed.
	Astrometry *model.Astrometry `json:"astrometry,omitempty"`
}

func toFrameDTO(f model.Frame) frameDTO {
	return frameDTO{
		ID: f.ID, RigID: f.RigID, SourceShare: f.SourceShare, SourcePath: f.SourcePath,
		SHA256: f.SHA256, Size: f.Size, Mtime: f.Mtime, FrameType: f.FrameType,
		Target: f.Target, Filter: f.Filter, Exptime: f.Exptime, SetTemp: f.SetTemp,
		Gain: f.Gain, Offset: f.Offset, Binning: f.Binning, Sensor: f.Sensor,
		DateObs: f.DateObs, Night: f.Night, DestPath: f.DestPath, Sidecars: f.Sidecars,
		State: string(f.State), Attempts: f.Attempts, NextRetryAt: f.NextRetryAt,
		PulledAt: f.PulledAt, DeletedAt: f.DeletedAt, Error: f.Error,
		MissingSeenAt: f.MissingSeenAt, RestoreRequestedAt: f.RestoreRequestedAt,
		MissingAckedAt: f.MissingAckedAt,
		Astrometry:     f.Astrometry,
	}
}

const (
	framesPageSize     = 50
	recentFramesMax    = 100
	recentFramesSize   = 25
	historyMaxPageSize = 200
)

// handleFrames serves a frame list (design architecture §8/§9: bulk history
// always comes from paginated REST, never the stream). Two modes share the
// endpoint:
//   - default: paginated, oldest-first, filterable by state/filter. page is
//     1-indexed in the API and 0-indexed in the store.
//   - ?limit=N: the newest N frames for the rig, most-recent first, across every
//     state — the "recent transfers" view. N is clamped to [1, recentFramesMax].
func (s *Server) handleFrames(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if raw := r.URL.Query().Get("limit"); raw != "" {
		s.handleRecentFrames(w, r, id, atoiDefault(raw, recentFramesSize))
		return
	}

	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}

	// ?missing=true routes to the observed-missing panel. It OVERRIDES state/filter
	// (a missing frame keeps its real state, so intersecting would surprise), and
	// carries the true per-directory group counts alongside the page.
	if r.URL.Query().Get("missing") == "true" {
		if r.URL.Query().Get("state") != "" || r.URL.Query().Get("filter") != "" {
			writeError(w, http.StatusBadRequest, "missing cannot combine with state/filter")
			return
		}
		// The panel returns every missing row so it stays a superset of the
		// safe-to-power-off banner (which counts any marked-missing row, no age
		// cutoff). missing_display_period's declutter belongs client-side (design
		// library-reconciliation §4.5/§4.7), not as a server-side row exclusion.
		// section splits the rows into actionable and leftover; dir and q narrow
		// the table only, never the folder stack (design library-reconciliation
		// §13.2).
		var section store.MissingSection
		switch raw := r.URL.Query().Get("section"); raw {
		case "", string(store.SectionActionable):
			section = store.SectionActionable
		case string(store.SectionLeftover):
			section = store.SectionLeftover
		default:
			writeError(w, http.StatusBadRequest, "section must be actionable or leftover")
			return
		}
		filter := store.MissingFilter{
			Section: section, Dir: r.URL.Query().Get("dir"), Q: r.URL.Query().Get("q"),
		}
		rows, total, err := s.deps.Store.ListMissingFrames(r.Context(), id, filter, page-1, framesPageSize)
		if err != nil {
			s.log.Error("web: list missing frames failed", "rig", id, "section", section, "err", err)
			writeError(w, http.StatusInternalServerError, "could not list missing frames")
			return
		}
		groups, totals, err := s.deps.Store.MissingGroupCounts(r.Context(), id, section)
		if err != nil {
			s.log.Error("web: missing group counts failed", "rig", id, "section", section, "err", err)
			writeError(w, http.StatusInternalServerError, "could not count missing frames")
			return
		}
		frames := make([]frameDTO, 0, len(rows))
		for _, f := range rows {
			frames = append(frames, toFrameDTO(f))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"frames": frames, "page": page, "page_size": framesPageSize, "total": total,
			"section": section, "groups": groups, "totals": totals,
		})
		return
	}

	state := r.URL.Query().Get("state")
	filter := r.URL.Query().Get("filter")

	rows, total, err := s.deps.Store.ListFrames(r.Context(), id, state, filter, page-1, framesPageSize)
	if err != nil {
		s.log.Error("web: list frames failed", "rig", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not list frames")
		return
	}

	frames := make([]frameDTO, 0, len(rows))
	for _, f := range rows {
		frames = append(frames, toFrameDTO(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"frames": frames, "page": page, "page_size": framesPageSize, "total": total,
	})
}

// handleRecentFrames answers the ?limit=N recency mode: the newest N frames for a
// rig, most-recent first, in the same envelope as the paginated list.
func (s *Server) handleRecentFrames(w http.ResponseWriter, r *http.Request, id string, limit int) {
	if limit < 1 {
		limit = 1
	}
	if limit > recentFramesMax {
		limit = recentFramesMax
	}

	rows, err := s.deps.Store.RecentFrames(r.Context(), id, limit)
	if err != nil {
		s.log.Error("web: recent frames failed", "rig", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not list frames")
		return
	}

	frames := make([]frameDTO, 0, len(rows))
	for _, f := range rows {
		frames = append(frames, toFrameDTO(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"frames": frames, "page": 1, "page_size": limit, "total": len(frames),
	})
}

// historyViewNeverDeletable is the ?view= of the transfer history that lists a
// rig's never-deletable frames (store ListNeverDeletable) instead of its
// transfers.
const historyViewNeverDeletable = "never_deletable"

// handleTransferHistory serves the combined, non-rig-scoped transfer history: every
// frame actually pulled to the NAS, newest-first, paginated and filterable by rig
// and/or a pulled_at window (design architecture §8: bulk history always comes
// from paginated REST). ?view=never_deletable lists the frames behind the backlog
// alert instead (design architecture §10). page is 1-indexed in the API and 0-indexed in the store;
// page_size defaults to framesPageSize and is clamped to historyMaxPageSize.
func (s *Server) handleTransferHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rig := q.Get("rig")
	from := int64(atoiDefault(q.Get("from"), 0))
	to := int64(atoiDefault(q.Get("to"), 0))
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	pageSize := atoiDefault(q.Get("page_size"), framesPageSize)
	if pageSize < 1 {
		pageSize = framesPageSize
	}
	if pageSize > historyMaxPageSize {
		pageSize = historyMaxPageSize
	}

	var rows []model.Frame
	var total int
	var err error
	switch view := q.Get("view"); view {
	case "":
		rows, total, err = s.deps.Store.ListTransferHistory(r.Context(), rig, from, to, page-1, pageSize)
	case historyViewNeverDeletable:
		// The frames behind the backlog alert, which links here. They are a state,
		// not a transfer window, so from/to do not apply (a failed frame was never
		// pulled at all).
		rows, total, err = s.deps.Store.ListNeverDeletable(r.Context(), rig, page-1, pageSize)
	default:
		writeError(w, http.StatusBadRequest, "unknown history view "+strconv.Quote(view))
		return
	}
	if err != nil {
		s.log.Error("web: transfer history failed", "rig", rig, "err", err)
		writeError(w, http.StatusInternalServerError, "could not list transfer history")
		return
	}

	frames := make([]frameDTO, 0, len(rows))
	for _, f := range rows {
		frames = append(frames, toFrameDTO(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"frames": frames, "page": page, "page_size": pageSize, "total": total,
	})
}

// summaryGroup is one GROUP BY bucket of the morning summary: frames sharing a
// type/filter/exposure/gain/binning/temperature, with their count and total bytes.
type summaryGroup struct {
	FrameType string  `json:"frame_type"`
	Filter    string  `json:"filter,omitempty"`
	Exptime   float64 `json:"exptime"`
	Gain      int64   `json:"gain"`
	Offset    int64   `json:"offset"`
	Binning   string  `json:"binning,omitempty"`
	SetTemp   float64 `json:"set_temp"`
	Sensor    string  `json:"sensor,omitempty"`
	Count     int     `json:"count"`
	Bytes     int64   `json:"bytes"`
}

// handleSummary computes the header-derived morning summary (design architecture
// §9): frames grouped by capture parameters with counts and byte totals,
// optionally scoped to one night. The store exposes no GROUP BY, so this
// aggregates in the handler over the rig's frames (paged through the store,
// night-filtered here).
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nightFilter, hasNight := 0, false
	if v := r.URL.Query().Get("night"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "night must be an integer")
			return
		}
		nightFilter, hasNight = n, true
	}

	groups := map[string]*summaryGroup{}
	var totalFrames int
	var totalBytes int64

	const pageSize = 500
	for page := 0; ; page++ {
		rows, total, err := s.deps.Store.ListFrames(r.Context(), id, "", "", page, pageSize)
		if err != nil {
			s.log.Error("web: summary list frames failed", "rig", id, "err", err)
			writeError(w, http.StatusInternalServerError, "could not build summary")
			return
		}
		for _, f := range rows {
			if hasNight && f.Night != int64(nightFilter) {
				continue
			}
			key := fmt.Sprintf("%s|%s|%g|%d|%d|%s|%g|%s",
				f.FrameType, f.Filter, f.Exptime, f.Gain, f.Offset, f.Binning, f.SetTemp, f.Sensor)
			g := groups[key]
			if g == nil {
				g = &summaryGroup{
					FrameType: f.FrameType, Filter: f.Filter, Exptime: f.Exptime,
					Gain: f.Gain, Offset: f.Offset, Binning: f.Binning,
					SetTemp: f.SetTemp, Sensor: f.Sensor,
				}
				groups[key] = g
			}
			g.Count++
			g.Bytes += f.Size
			totalFrames++
			totalBytes += f.Size
		}
		if (page+1)*pageSize >= total || len(rows) == 0 {
			break
		}
	}

	out := make([]summaryGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	resp := map[string]any{
		"rig_id": id, "groups": out, "total_frames": totalFrames, "total_bytes": totalBytes,
	}
	if hasNight {
		resp["night"] = nightFilter
	}
	writeJSON(w, http.StatusOK, resp)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
