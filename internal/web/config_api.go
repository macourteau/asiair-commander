package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
	"golang.org/x/crypto/bcrypt"
)

// handleGetConfig returns the live config with every secret redacted (design
// architecture §8). The response carries updated_at, the optimistic-concurrency
// token a subsequent PUT must echo.
//
// It also carries any findings about the STORED address list, so an install that
// was hand-edited to something like `allowed_hosts: ["*"]` says so the moment the
// editor is opened rather than only after a save. Every stored entry is
// grandfathered by definition, so these are always warnings.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	stored := s.currentConfig()
	writeJSON(w, http.StatusOK, configResponse{
		Config:   config.Redact(stored),
		Problems: config.ValidateSubmittedHosts(stored.Web.AllowedHosts, stored.Web.AllowedHosts),
	})
}

// handlePutConfig validates and applies a full config PUT. The body is a whole
// model.Config carrying either real values or the redaction sentinel for secrets;
// the write pipeline (saveConfig) enforces optimistic concurrency, restores
// sentinel'd secrets, hashes a changed password, validates, saves, applies, and
// swaps the live pointer. It returns the new redacted config.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var put configPut
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if err := decodeJSON(bytes.NewReader(body), &put); err != nil {
		// A unit-less duration fails inside json.Decode before Validate runs, losing
		// the field path. Re-scan the raw body to name the offending duration so it
		// surfaces inline like every other validation problem.
		if problems := config.LocalizeDecodeError(body, err); len(problems) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid configuration", "problems": problems,
			})
			return
		}
		writeError(w, http.StatusBadRequest, "invalid configuration body")
		return
	}
	// A stale write is still caught by CheckVersion inside the guard, so reading
	// the stored config here — as the address-list check below already does — can
	// only ever preserve a value a rejected write would have overwritten anyway.
	incoming := *put.merge(s.currentConfig())
	// Address-list rules apply to what this request introduces, never to what is
	// already stored — see config.ValidateSubmittedHosts. Run on the raw body,
	// before normalization destroys the shapes the scheme rule rejects.
	hostProblems := config.ValidateSubmittedHosts(
		incoming.Web.AllowedHosts, s.currentConfig().Web.AllowedHosts,
	)
	if config.HasErrors(hostProblems) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid configuration", "problems": hostProblems,
		})
		return
	}
	if !s.checkAllowlistLockout(w, r, &incoming) {
		return
	}
	merged, ok := s.saveConfig(w, &incoming)
	if !ok {
		return
	}
	// A grandfathered entry ("*", a scheme-bearing string) is a warning, not an
	// error, so the save succeeds — but the whole point of downgrading it is that
	// the operator still gets told, so the warnings ride the 200 body. Dropping
	// them here would let a hand-edited allowlist live forever without a signal.
	writeJSON(w, http.StatusOK, configResponse{
		Config:   config.Redact(merged),
		Problems: hostProblems,
	})
}

// configPut is the decode target for a caller-supplied whole config. It is a
// plain model.Config plus pointer shadows for the two fields whose absence must
// not read as "set me to the zero value".
//
// A client older than the server omits both. mosaic_detection would then decode
// as false and silently reverse the operator's opt-in — a stale tab pressing Save
// would start filing panels flat again, by a path no UI warning can see — and
// templates.mosaic would decode as "", which normalization fills with the DEFAULT
// rather than the customised value that was stored.
//
// The shadow has to capture the whole templates object, not a bare *string named
// mosaic: templates is nested, so a depth-0 field of that name would be a sibling
// of templates and intercept nothing. Because it does capture it, the embedded
// Config's own Templates decodes zero-valued and every sub-field is projected
// back in merge.
type configPut struct {
	model.Config
	MosaicDetection *bool `json:"mosaic_detection"`
	Templates       *struct {
		Light  *string                    `json:"light"`
		Cal    map[model.FrameType]string `json:"cal"`
		Mosaic *string                    `json:"mosaic"`
	} `json:"templates"`
}

// merge projects the decoded payload onto a whole config, taking the stored
// value for anything the payload left out.
func (p *configPut) merge(stored *model.Config) *model.Config {
	cfg := p.Config

	cfg.MosaicDetection = stored.MosaicDetection
	if p.MosaicDetection != nil {
		cfg.MosaicDetection = *p.MosaicDetection
	}

	// The cal map is cloned rather than aliased: NormalizeTemplates rewrites it in
	// place, which would otherwise reach into the live config the rest of the
	// request still reads as "stored".
	cfg.Templates = model.Templates{
		Light:  stored.Templates.Light,
		Cal:    cloneCalTemplates(stored.Templates.Cal),
		Mosaic: stored.Templates.Mosaic,
	}
	if p.Templates != nil {
		if p.Templates.Light != nil {
			cfg.Templates.Light = *p.Templates.Light
		}
		if p.Templates.Cal != nil {
			cfg.Templates.Cal = cloneCalTemplates(p.Templates.Cal)
		}
		if p.Templates.Mosaic != nil {
			cfg.Templates.Mosaic = *p.Templates.Mosaic
		}
	}
	return &cfg
}

func cloneCalTemplates(in map[model.FrameType]string) map[model.FrameType]string {
	if in == nil {
		return nil
	}
	out := make(map[model.FrameType]string, len(in))
	for t, tmpl := range in {
		out[t] = tmpl
	}
	return out
}

// configResponse is a redacted config plus any non-blocking findings about it.
// The config is embedded, not nested, so the body keeps the bare-config shape
// every existing client parses; `problems` is an added key an older SPA ignores.
type configResponse struct {
	*model.Config
	Problems []config.Problem `json:"problems,omitempty"`
}

// checkAllowlistLockout refuses a config PUT whose address list would no longer
// admit the browser making the request, naming the entry that has to stay.
//
// It tests the checkOrigin rule, not checkHost. checkHost auto-admits private IP
// literals while checkOrigin does not, so a LAN-IP operator who deletes their own
// entry would look fine here and then find every mutation refused — reads working
// and writes failing is the worst shape this can take.
//
// It is a usability check, NOT a security control, because it is computed from a
// client-supplied header: any client that lies satisfies it. It also does not
// cover removing the only entry other devices use, or a web.port change in the
// same PUT. It exists to catch the honest mistake.
//
// It lives here rather than in saveConfigLocked because that helper is shared
// with the rig CRUD handlers and with the logout secret rotation, none of which
// should acquire a request-dependent guard, and it has no request to compute one
// from. It runs on the decoded body before ApplyRedactedWrite, so a PUT that
// omitted web.allowed_hosts entirely would trip it — unreachable while the
// SPA always sends whole configs, but written down so a future partial PUT does
// not rediscover it as a bug.
//
// The wizard's final PUT is not a special case: it sends the config the setup
// response returned, which already carries the seeded origin, so this passes by
// construction. A guard that could refuse it would be refusing the operator's own
// address.
func (s *Server) checkAllowlistLockout(w http.ResponseWriter, r *http.Request, next *model.Config) bool {
	host, _ := originHost(r)
	if host == "" {
		host = hostOnly(r.Host)
	}
	if host == "" || s.allowedHosts(next)[strings.ToLower(host)] {
		return true
	}
	s.log.Warn("web: config write refused, it would remove the requesting address",
		"origin_host", host)
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": "invalid configuration",
		"problems": []config.Problem{{
			Severity: config.SeverityError,
			Field:    "web.allowed_hosts",
			Message: fmt.Sprintf("keep %q in the list: it is the address you are reaching "+
				"Commander at, and removing it would lock you out of these settings", host),
		}},
	})
	return false
}

// saveConfig is the shared write path for every config mutation (PUT /api/config
// and the rig CRUD endpoints). It:
//
//  1. rejects a stale write with 409 (CheckVersion),
//  2. restores sentinel'd secrets from the stored config (ApplyRedactedWrite),
//  3. hashes a newly-set web password (a plaintext arrives in PasswordHash; an
//     unchanged field arrived as the sentinel and is already restored, so a value
//     differing from the stored hash is the new plaintext) and never clears a set
//     password,
//  4. regenerates a session secret only if one is somehow empty,
//  5. rejects validation errors with 400 + the []Problem list,
//  6. saves atomically, applies to the supervisor, and swaps the live pointer.
//
// On any failure it writes the response and returns ok=false.
func (s *Server) saveConfig(w http.ResponseWriter, candidate *model.Config) (*model.Config, bool) {
	// Serialize the whole read-modify-write behind the shared config guard so two
	// concurrent writers can't interleave (this also closes the pre-existing TOCTOU
	// that the UpdatedAt token alone did not). Every response, secret restore, and
	// Save→Apply→Store step below is unchanged — only mutual exclusion is added.
	var result *model.Config
	var ok bool
	_ = s.deps.Guard.Do(func() error {
		result, ok = s.saveConfigLocked(w, candidate)
		return nil
	})
	return result, ok
}

// saveConfigLocked is saveConfig's body, run inside the config guard.
func (s *Server) saveConfigLocked(w http.ResponseWriter, candidate *model.Config) (*model.Config, bool) {
	stored := s.currentConfig()

	if err := config.CheckVersion(stored, candidate); err != nil {
		s.log.Warn("web: config write rejected as stale",
			"incoming_updated_at", candidate.UpdatedAt, "stored_updated_at", stored.UpdatedAt)
		writeError(w, http.StatusConflict,
			"configuration changed since you loaded it; reload and reapply your edit")
		return nil, false
	}

	merged := config.ApplyRedactedWrite(stored, candidate)

	passwordChanged := false
	switch {
	case merged.PasswordHash == "" && stored.PasswordHash != "":
		// Never clear a set password (that would reopen the setup takeover window).
		merged.PasswordHash = stored.PasswordHash
	case merged.PasswordHash != "" && merged.PasswordHash != stored.PasswordHash:
		cost := merged.Web.BcryptCost
		if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
			cost = 12
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(merged.PasswordHash), cost)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not hash password")
			return nil, false
		}
		merged.PasswordHash = string(hash)
		passwordChanged = true
	}

	// Canonicalize folder templates to "/" before validation and save so a
	// "\"-authored template (an operator on Windows) is stored host-independently.
	config.NormalizeTemplates(merged)
	config.NormalizeAllowedHosts(merged)

	if merged.SessionSecret == "" {
		merged.SessionSecret = genSecret()
	}
	if passwordChanged {
		// A password change revokes every existing session: install a fresh secret
		// so cookies signed with the old one stop verifying. The current admin is
		// re-issued a cookie after the swap below so they are not logged out by
		// their own change.
		merged.SessionSecret = genSecret()
	}

	if problems := config.Validate(merged, s.volumeResolver()); config.HasErrors(problems) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid configuration", "problems": problems,
		})
		return nil, false
	}

	if problems := s.disableGuardProblems(merged, stored); len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid configuration", "problems": problems,
		})
		return nil, false
	}

	if err := config.Save(s.deps.ConfigPath, merged); err != nil {
		s.log.Error("web: config save failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not save configuration")
		return nil, false
	}
	s.deps.Supervisor.ApplyConfig(merged)
	s.deps.Config.Store(merged)
	s.log.Info("web: config applied", "updated_at", merged.UpdatedAt)

	if passwordChanged {
		now := s.now()
		fresh := sessionClaims{IssuedAt: now.Unix(), Seen: now.Unix(), XSRF: genToken()}
		if err := s.setSessionCookies(w, merged, fresh); err != nil {
			writeError(w, http.StatusInternalServerError, "could not refresh session after password change")
			return nil, false
		}
	}
	return merged, true
}

// disableGuardProblems enforces the disable-guard safety rule (design
// device-identity-and-media §8.2): a share may only be newly disabled when the
// rig's most recent confirmed listing found its removable slot STRUCTURALLY empty
// (no medium, so no frames to strand). It diffs the merged candidate against the
// stored config and, for every source disabled in the candidate but not already
// disabled in the current config — which captures an edit, a create, a
// remove-and-re-add, and a config-PUT insert — consults the rig's live published
// status and refuses unless the rig is online, not paused, its last successful
// listing is within one poll interval, and the share is in that rig's
// structurally-confirmed-empty set. A brand-new or offline rig has no confirming
// listing, so a pre-disabled source is refused: add it enabled, let one listing
// confirm the slot empty, then disable it. Refusals ride the per-field problems[]
// channel (design architecture §8.8) on rigs[<id>].sources so the toggle's
// rejection renders inline.
func (s *Server) disableGuardProblems(candidate, stored *model.Config) []config.Problem {
	var snap model.Snapshot
	if s.deps.Hub != nil {
		snap = s.deps.Hub.Snapshot()
	}
	now := s.now().Unix()
	var problems []config.Problem
	for i := range candidate.Rigs {
		rig := &candidate.Rigs[i]
		for _, src := range rig.Sources {
			if !src.Disabled || sourceAlreadyDisabled(stored.Rig(rig.ID), src.Share) {
				// Only a fresh introduction of a disabled state is confirmed; an
				// already-disabled source passes untouched.
				continue
			}
			if msg := confirmDisableAllowed(snap, rig.ID, src.Share, now); msg != "" {
				problems = append(problems, config.Problem{
					Severity: config.SeverityError,
					Field:    fmt.Sprintf("rigs[%s].sources", rig.ID),
					Message:  msg,
				})
			}
		}
	}
	return problems
}

// sourceAlreadyDisabled reports whether the stored rig already carries a disabled
// source for this share, so disabling it is not a fresh introduction and needs no
// re-confirmation.
func sourceAlreadyDisabled(stored *model.RigProfile, share string) bool {
	if stored == nil {
		return false
	}
	for _, s := range stored.Sources {
		if s.Share == share && s.Disabled {
			return true
		}
	}
	return false
}

// confirmDisableAllowed returns "" when the rig's live status confirms the share's
// slot is structurally empty (safe to disable), or a human-readable refusal message
// otherwise. ListingTS is stamped only on a successful listing, so the freshness
// bound (within one poll) rejects an offline/paused/stale rig whose empty-slot set
// is frozen; a non-positive PollIntervalSec (cadence unknown) also refuses.
func confirmDisableAllowed(snap model.Snapshot, rigID, share string, now int64) string {
	var st *model.RigStatus
	for i := range snap.Rigs {
		if snap.Rigs[i].ID == rigID {
			st = &snap.Rigs[i]
			break
		}
	}
	if st == nil || !st.Online {
		return fmt.Sprintf("Bring the rig online so we can confirm '%s' is empty before disabling it.", share)
	}
	if st.Paused {
		return fmt.Sprintf("Resume the rig so we can confirm '%s' is empty before disabling it.", share)
	}
	if st.PollIntervalSec <= 0 || now-st.ListingTS > int64(st.PollIntervalSec) {
		return fmt.Sprintf("Wait for a fresh check to confirm '%s' is empty before disabling it.", share)
	}
	for _, empty := range st.EmptySlots {
		if empty == share {
			return ""
		}
	}
	return fmt.Sprintf("Can't disable '%s' while a medium is inserted — remove it (or delete the share) first.", share)
}

// rotateSessionSecret installs a fresh session secret and persists it. Every
// outstanding cookie was signed with the old secret, so this revokes them all at
// once — the mechanism behind logout and password-change invalidation, so a
// captured cookie cannot outlive the action meant to void it.
func (s *Server) rotateSessionSecret() error {
	// The logout rotation is a config write like any other, so it runs inside the
	// shared guard; outside it, it would race saveConfig.
	return s.deps.Guard.Do(func() error {
		cfg, err := deepCopyConfig(s.currentConfig())
		if err != nil {
			return err
		}
		cfg.SessionSecret = genSecret()
		if err := config.Save(s.deps.ConfigPath, cfg); err != nil {
			return err
		}
		s.deps.Supervisor.ApplyConfig(cfg)
		s.deps.Config.Store(cfg)
		return nil
	})
}

// volumeResolver adapts the injected LocalFS to config.VolumeResolver (its
// SameVolume signature matches). A nil LocalFS yields a nil resolver, which
// Validate treats as "skip the single-volume check".
func (s *Server) volumeResolver() config.VolumeResolver {
	if s.deps.LocalFS == nil {
		return nil
	}
	return s.deps.LocalFS
}

// deepCopyConfig clones a config via a JSON round-trip so a rig mutation can edit
// a copy without touching the live value the pipeline still reads as "stored".
func deepCopyConfig(c *model.Config) (*model.Config, error) {
	if c == nil {
		return nil, errors.New("nil config")
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	var out model.Config
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &out, nil
}
