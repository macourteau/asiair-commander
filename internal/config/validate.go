package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/route"
)

// tokenPattern matches a single <...> template token.
var tokenPattern = regexp.MustCompile(`<[^>]*>`)

// canonicalTokens is the full set of template substitution tokens (design
// routing-and-filing §3). Any <...> outside this set is a typo that would
// otherwise render as a literal folder name, so a template containing one is
// rejected at save.
var canonicalTokens = map[string]bool{
	"<TARGET>": true, "<OTA>": true, "<SESSION>": true, "<n>": true,
	"<SENSOR>": true, "<DATE>": true, "<SET-TEMP>": true, "<EXPOSURE>": true,
	"<FILTER>": true, "<GAIN>": true, "<OFFSET>": true, "<BINNING>": true,
	"<RIG>": true, "<YEAR>": true, "<MONTH>": true, "<DAY>": true,
	"<TARGET_BASE>": true, "<PANEL>": true, "<PANEL_ROW>": true, "<PANEL_COL>": true,
}

// mosaicOnlyTokens are the tokens only the mosaic template may name. Elsewhere
// they have no value to render — nothing outside a mosaic panel has a base or a
// pane index — so the router quarantines a frame rather than filing it into a
// folder literally called "_PANEL_", and saving one anywhere else is refused
// here so the mistake never reaches routing. <TARGET_BASE> is restricted for a
// second reason: in the light template it would be indistinguishable from
// <TARGET>, since panels never route through the light template.
var mosaicOnlyTokens = map[string]bool{
	"<TARGET_BASE>": true, "<PANEL>": true, "<PANEL_ROW>": true, "<PANEL_COL>": true,
}

// templateScope names which template field validateTemplate is checking. Only
// two rules consult it — the mosaic-only tokens, which are refused outside the
// mosaic field, and the mosaic-specific emptiness and panel-distinguishing
// rules. Acceptance of every other token stays global:
// the SPA scopes tokens per kind client-side, and tightening the server to match
// would reject templates operators already have saved.
type templateScope string

const (
	scopeLight  templateScope = "light"
	scopeCal    templateScope = "cal"
	scopeMosaic templateScope = "mosaic"
)

// Severity levels for a validation Problem.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Problem is one validation finding. Severity is SeverityError (config is
// invalid) or SeverityWarning (config is usable but risky). Field names the
// offending area; Message is human-readable (design routing-and-filing, design
// architecture §7, §8).
type Problem struct {
	Severity string
	Field    string
	Message  string
}

// VolumeResolver answers whether a set of paths resolve to one filesystem volume.
// It is injected so Validate stays unit-testable; a nil resolver skips the
// one-volume check (design architecture §7 "one managed destination volume").
type VolumeResolver interface {
	SameVolume(paths ...string) (bool, error)
}

// HostResolver optionally resolves a rig host to a comparable address. If the
// VolumeResolver passed to Validate also implements HostResolver, the
// duplicate-host warning resolves hostnames before comparing; otherwise the
// comparison falls back to the literal host strings. Folding it into the same
// injected value keeps Validate's signature stable while remaining testable.
type HostResolver interface {
	Resolve(host string) (string, error)
}

// maxPathBudget is the legacy Windows MAX_PATH ceiling. Extended-length (\\?\)
// paths lift it at runtime, but a template whose plausible realized path exceeds
// it is still worth a warning (design routing-and-filing §4 long-path safety).
const maxPathBudget = 260

// plausibleSegmentLen is the per-token length assumed when estimating a template's
// worst-case realized path, and plausibleFilenameLen the assumed leaf filename
// length. They are deliberately moderate ("a plausible frame", not the hard cap)
// so genuinely deep trees warn while ordinary ones do not.
const (
	plausibleSegmentLen  = 20
	plausibleFilenameLen = 48
)

// scienceExtensions are the transferred science-frame extensions a
// delete_sidecar_extension must never overlap — deleting one would delete the
// frame itself (design frame-lifecycle §3).
var scienceExtensions = map[string]bool{"fit": true, "fits": true, "xisf": true}

// NormalizeTemplates rewrites every folder template to the canonical
// forward-slash separator. Templates may be authored with either separator (an
// operator on Windows types "\"), but they are stored "/"-joined so the on-disk
// config stays host-independent and the router's segment split is unambiguous.
// The router also accepts either separator at render time, so this is a
// normalization for storage and display, not a correctness gate.
//
// It also fills a blank mosaic template with the shipped default. That field
// arrived after the config schema and is simply absent on every install that
// predates it, so validation would otherwise see "" — and an empty template is
// refused. Filling it here rather than in a migration means no stored config is
// rewritten in order to be migrated, which matters because the default names
// <OTA> and <SESSION>: writing it into an install that leaves those blank would
// invalidate a config that is otherwise valid. The way to opt out of mosaic
// handling is the mosaic_detection toggle, not a blank template.
func NormalizeTemplates(cfg *model.Config) {
	cfg.Templates.Light = strings.ReplaceAll(cfg.Templates.Light, `\`, "/")
	for t, tmpl := range cfg.Templates.Cal {
		cfg.Templates.Cal[t] = strings.ReplaceAll(tmpl, `\`, "/")
	}
	if strings.TrimSpace(cfg.Templates.Mosaic) == "" {
		cfg.Templates.Mosaic = defaultMosaicTemplate
	}
	cfg.Templates.Mosaic = strings.ReplaceAll(cfg.Templates.Mosaic, `\`, "/")
}

// hostnamePattern matches a hostname label sequence: labels of letters, digits,
// internal hyphens and underscores, joined by dots. Entries are lower-cased by
// NormalizeHostEntry before they reach it.
//
// Underscores are permitted even though RFC 1123 forbids them, because consumer
// DHCP and NetBIOS names routinely carry them and this rule's job is catching
// garbage that could never match a Host header — schemes, paths, whitespace,
// wildcards — not enforcing RFC 1123. checkHost and checkOrigin compare the
// stored entry to the browser's value as an exact string, so "my_nas" matches
// precisely what that browser sends and is harmless. Rejecting it here while
// seedRequestOrigin accepts the same name from the same operator's Origin would
// be incoherent: both paths feed the one exact-match map.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?)*$`)

// ValidateSubmittedHosts reports problems with web.allowed_hosts entries a
// request is introducing. It is deliberately NOT part of Validate: Validate runs
// on *stored* configs (the config PUT pipeline, setup, and the manager's
// migration applier), and this field has never been validated, so hand-edited
// values like "*" or "http://nas.local" are the population in the wild. Enforcing
// these rules there would 400 every config PUT and every rig save, and fail
// bindPendingGUID permanently with "migrated config invalid", on a field the
// operator never touched.
//
// It therefore diffs: an entry already present in stored is grandfathered and
// downgraded to a warning, so the Settings editor can surface it for repair
// without blocking the operator's unrelated edit. Diffing is required rather
// than a refinement, because the SPA PUTs whole configs and the GET returns
// allowed_hosts, so an install storing "*" round-trips it straight back.
//
// It runs on the RAW submitted strings, before normalization: normalizing first
// would destroy the very shapes the scheme rule exists to reject, so
// "https://nas.local" typed into the editor would be quietly turned into "https"
// instead of producing the promised error.
func ValidateSubmittedHosts(submitted, stored []string) []Problem {
	grandfathered := make(map[string]bool, len(stored))
	for _, h := range stored {
		if n := NormalizeHostEntry(h); n != "" {
			grandfathered[n] = true
		}
	}

	var problems []Problem
	seen := map[string]bool{}
	for _, raw := range submitted {
		normalized := NormalizeHostEntry(raw)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true

		severity := SeverityError
		if grandfathered[normalized] {
			severity = SeverityWarning
		}
		if msg := hostEntryFault(raw, normalized); msg != "" {
			problems = append(problems, Problem{
				Severity: severity,
				Field:    "web.allowed_hosts",
				Message:  fmt.Sprintf("%q %s", raw, msg),
			})
			continue
		}
		// An IP literal warns but never blocks: DHCP churn eventually invalidates
		// it, and checkOrigin has no private-IP escape hatch, so an allowlist of
		// only IP literals leaves the operator half-broken after a lease change —
		// reads pass the Host check and every mutation is refused.
		if _, err := netip.ParseAddr(normalized); err == nil {
			problems = append(problems, Problem{
				Severity: SeverityWarning,
				Field:    "web.allowed_hosts",
				Message: fmt.Sprintf("%q is an IP address, which can change when the DHCP lease renews; "+
					"add a hostname as well so you do not lose access", raw),
			})
		}
	}
	return problems
}

// hostEntryFault returns why a submitted allowlist entry is unusable, or "".
func hostEntryFault(raw, normalized string) string {
	if strings.ContainsAny(raw, "*?") {
		// Inert while the allowlist is an exact-match map: "*" matches only the
		// literal string. Rejected as future-proofing: the day anyone makes that map
		// pattern-aware, a stored wildcard becomes a silent DNS-rebinding bypass.
		return "contains a wildcard, which is not supported; list each address in full"
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "contains whitespace, so it can never match a Host header"
	}
	if strings.Contains(raw, "://") || strings.Contains(raw, "/") || strings.Contains(raw, "@") {
		return "looks like a URL; list only the host, e.g. nas.local"
	}
	if _, err := netip.ParseAddr(normalized); err == nil {
		return ""
	}
	if !hostnamePattern.MatchString(normalized) || len(normalized) > 253 {
		return "is not a valid hostname or IP address, so it can never match a Host header"
	}
	return ""
}

// AcceptableSeededHost reports whether a host the server seeds itself — the
// requesting browser's origin — is safe to store. It applies the wildcard,
// scheme, and whitespace rules but not the hostname-shape rule.
//
// The exemption is narrow on purpose. Origin is a free-text header a non-browser
// client can set to anything, so the rules that reject values which could never
// be a real host still apply. What is waived is the shape check, because this
// value demonstrably reached the server from the browser the operator is using:
// refusing an odd-looking name here would not fail the setup POST, it would fail
// the *next* call, with the password already committed.
func AcceptableSeededHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.ContainsAny(host, "*?/@ \t\r\n") || strings.Contains(host, "://") {
		return false
	}
	return true
}

// HasErrors reports whether any problem is a hard error.
func HasErrors(problems []Problem) bool {
	for _, p := range problems {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Validate checks cfg against the design routing-and-filing and design
// architecture §7/§8 rules and returns every problem found (errors and warnings
// both); the caller decides what to block on. A nil vol skips the one-volume root
// check so the function is usable without a filesystem.
func Validate(cfg *model.Config, vol VolumeResolver) []Problem {
	var ps []Problem

	ps = append(ps, validateRoots(cfg, vol)...)
	ps = append(ps, validateGrace(cfg)...)
	ps = append(ps, validateReconcileSettle(cfg)...)
	ps = append(ps, validateCadence(cfg)...)
	ps = append(ps, validateExtensions(cfg)...)
	ps = append(ps, validateTemplates(cfg)...)
	ps = append(ps, validateMisc(cfg)...)
	ps = append(ps, validateSources(cfg)...)
	ps = append(ps, validateHosts(cfg, vol)...)
	ps = append(ps, validateBinding(cfg)...)
	ps = append(ps, validateClockSync(cfg)...)
	ps = append(ps, validateRemotes(cfg)...)

	return ps
}

// validateRemotes checks the remote profiles. Only the remote supervisor writes
// them, so a problem here is a hand edit or a bug; it is still caught before the
// config is saved. The id must name a site, since it is one.
func validateRemotes(cfg *model.Config) []Problem {
	var ps []Problem
	seen := map[string]bool{}
	for _, r := range cfg.Remotes {
		field := fmt.Sprintf("remotes[%s]", r.ID)
		switch {
		case !netsite.ValidID(netsite.ID(r.ID)):
			ps = append(ps, Problem{SeverityError, field + ".id",
				fmt.Sprintf("%q is not a valid remote id (letters, digits, '-' and '_' only)", r.ID)})
		case seen[r.ID]:
			ps = append(ps, Problem{SeverityError, field + ".id", "two remotes share this id"})
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Name) == "" {
			ps = append(ps, Problem{SeverityError, field + ".name", "must not be empty"})
		}
		if r.Address == "" || r.ClientKey == "" {
			ps = append(ps, Problem{SeverityError, field, "is missing its pairing; make new setup files for it on its card"})
		}
		if r.RateLimitMBps < 0 {
			ps = append(ps, Problem{SeverityError, field + ".rate_limit_mbps", "must not be negative"})
		}
	}
	return ps
}

// validateBinding enforces the device-binding rules (design architecture §8). A
// managed rig must bind to something — a durable GUID, or (only pre-migration) a
// legacy Host — so a profile that resolves to no device is caught at save. Two
// managed profiles must never share a non-empty GUID (a clone or double-adopt),
// which would race two workers onto one device.
func validateBinding(cfg *model.Config) []Problem {
	var ps []Problem
	seenGUID := make(map[string]string) // guid -> first rig id
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if !r.Managed {
			continue
		}
		if strings.TrimSpace(r.GUID) == "" && strings.TrimSpace(r.Host) == "" {
			ps = append(ps, Problem{SeverityError,
				fmt.Sprintf("rigs[%s].guid", r.ID),
				"a managed rig must bind to a device — adopt it from the Devices page (or, temporarily, set an address)"})
		}
		if r.GUID != "" {
			if first, ok := seenGUID[r.GUID]; ok {
				ps = append(ps, Problem{SeverityError,
					fmt.Sprintf("rigs[%s].guid", r.ID),
					fmt.Sprintf("binds the same device guid as rig %q; two profiles cannot manage one device", first)})
				continue
			}
			seenGUID[r.GUID] = r.ID
		}
	}
	return ps
}

// validateClockSync surfaces a clock-sync opt-in that can never take effect.
// Setting a device's clock is only ever done to a rig whose identity is confirmed
// on the control channel itself, which needs the profile's guid; a host-bound
// profile would otherwise be skipped silently every cycle.
func validateClockSync(cfg *model.Config) []Problem {
	var ps []Problem
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if !r.SyncClock || strings.TrimSpace(r.GUID) != "" {
			continue
		}
		ps = append(ps, Problem{SeverityWarning,
			fmt.Sprintf("rigs[%s].sync_clock", r.ID),
			"clock sync needs an adopted, guid-bound rig — it never runs for a host-bound profile"})
	}
	return ps
}

func validateRoots(cfg *model.Config, vol VolumeResolver) []Problem {
	if vol == nil {
		return nil
	}
	same, err := vol.SameVolume(cfg.Roots.Staging, cfg.Roots.Light, cfg.Roots.Cal)
	if err != nil {
		return []Problem{{SeverityError, "roots", fmt.Sprintf("could not resolve destination volumes: %v", err)}}
	}
	if !same {
		return []Problem{{SeverityError, "roots",
			"staging, light, and cal roots must all resolve to one volume (cross-volume rename fails EXDEV)"}}
	}
	return nil
}

func validateGrace(cfg *model.Config) []Problem {
	var ps []Problem
	// Grace guards deletion: a re-read microseconds after a pull proves nothing, so
	// a rig that deletes needs an effective grace > 0 (design architecture §7/§16).
	// A keep-originals rig never runs the delete pass, so it has no grace to enforce
	// and is skipped.
	for i := range cfg.Rigs {
		if cfg.Rigs[i].KeepOriginals {
			continue
		}
		if cfg.EffectiveGrace(&cfg.Rigs[i]) <= 0 {
			ps = append(ps, Problem{SeverityError,
				fmt.Sprintf("rigs[%s].grace", cfg.Rigs[i].ID),
				"effective grace must be > 0 while deletion is enabled"})
		}
	}
	return ps
}

// validateReconcileSettle rejects a non-positive settle window. Unlike grace it is
// not skipped for keep-originals rigs: the settle also drives the missing panel's
// deletion-deadline display and the validation report's projection, which those
// rigs still surface. A zero/negative settle would authorize deletion the instant
// a mark is set, collapsing the reconciliation safety window.
func validateReconcileSettle(cfg *model.Config) []Problem {
	var ps []Problem
	for i := range cfg.Rigs {
		if cfg.EffectiveReconcileSettle(&cfg.Rigs[i]) <= 0 {
			ps = append(ps, Problem{SeverityError,
				fmt.Sprintf("rigs[%s].reconcile_settle", cfg.Rigs[i].ID),
				"effective reconcile_settle must be > 0"})
		}
	}
	return ps
}

// validateCadence rejects non-positive poll/retry intervals. A zero or negative
// interval would panic the worker's ticker outside its per-cycle recover and take
// down the whole service, so it must never reach the runtime.
func validateCadence(cfg *model.Config) []Problem {
	var ps []Problem
	if cfg.Defaults.PollInterval.D() <= 0 {
		ps = append(ps, Problem{SeverityError, "defaults.poll_interval", "poll interval must be > 0"})
	}
	if cfg.Defaults.RetryInterval.D() <= 0 {
		ps = append(ps, Problem{SeverityError, "defaults.retry_interval", "retry interval must be > 0"})
	}
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if r.PollInterval != nil && r.PollInterval.D() <= 0 {
			ps = append(ps, Problem{SeverityError,
				fmt.Sprintf("rigs[%s].poll_interval", r.ID), "poll interval must be > 0"})
		}
		if r.RetryInterval != nil && r.RetryInterval.D() <= 0 {
			ps = append(ps, Problem{SeverityError,
				fmt.Sprintf("rigs[%s].retry_interval", r.ID), "retry interval must be > 0"})
		}
	}
	return ps
}

func validateExtensions(cfg *model.Config) []Problem {
	var ps []Problem
	exclude := normExtSet(cfg.ExcludeExtensions)
	for _, raw := range cfg.DeleteSidecarExtensions {
		e := normExt(raw)
		if !exclude[e] {
			ps = append(ps, Problem{SeverityError, "delete_sidecar_extensions",
				fmt.Sprintf("%q must also be listed in exclude_extensions", raw)})
		}
		if scienceExtensions[e] {
			ps = append(ps, Problem{SeverityError, "delete_sidecar_extensions",
				fmt.Sprintf("%q is a science-frame extension and must not be a deletable sidecar", raw)})
		}
	}
	return ps
}

func validateTemplates(cfg *model.Config) []Problem {
	var ps []Problem
	ps = append(ps, validateTemplate("templates.light", cfg.Templates.Light, scopeLight, true, cfg.Roots.Light)...)
	for _, t := range model.CanonicalTypes {
		if t == model.TypeLight {
			continue
		}
		tmpl, ok := cfg.Templates.Cal[t]
		if !ok {
			continue
		}
		ps = append(ps, validateTemplate(fmt.Sprintf("templates.cal.%s", t), tmpl, scopeCal, false, cfg.Roots.Cal)...)
	}
	// The mosaic template is checked only while detection is on, and then in
	// full. Off, the field routes nothing and the Settings editor does not show
	// it, so enforcing it would let a hand-edited value — or one typed before the
	// toggle was turned off — reject every later config write over a field the
	// operator cannot see. Turning detection on is itself a save, so nothing
	// escapes: every rule below applies from that moment.
	//
	// It is scored against the LIGHT root because that is where a mosaic panel
	// lands; it carries a whole segment more than the light template, so an
	// install with deep roots can see a path-length warning here that its light
	// template does not draw.
	if cfg.MosaicDetection {
		ps = append(ps, validateTemplate("templates.mosaic", cfg.Templates.Mosaic, scopeMosaic, true, cfg.Roots.Light)...)
	}
	return ps
}

// validateTemplate enforces path-safety on a single template: no "..", no
// absolute prefix, no drive letter / colon. A requireNight template must contain
// the night segment token "<n>". It also warns on an unreasonable worst-case
// realized path length under root. scope carries which field this is, for the
// rules that are not the same everywhere (see templateScope).
func validateTemplate(field, tmpl string, scope templateScope, requireNight bool, root string) []Problem {
	var ps []Problem

	// Only the mosaic template is required to be non-empty. Making the rule
	// global would newly refuse a present-but-empty cal key — no such rule has
	// ever existed there — and 400 every config write on installs whose configs are
	// valid. Reported alone: every rule below would fire on "" as well, and a
	// list of five problems is worse guidance than the one that caused them.
	if scope == scopeMosaic && strings.TrimSpace(tmpl) == "" {
		return []Problem{{SeverityError, field,
			"mosaic template must not be empty — clear the field to restore the default, or turn off mosaic detection"}}
	}

	for _, seg := range strings.FieldsFunc(tmpl, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			ps = append(ps, Problem{SeverityError, field, `template must not contain a ".." segment`})
			break
		}
	}
	if strings.HasPrefix(tmpl, "/") || strings.HasPrefix(tmpl, "\\") {
		ps = append(ps, Problem{SeverityError, field, "template must be relative, not absolute"})
	}
	if strings.Contains(tmpl, ":") {
		ps = append(ps, Problem{SeverityError, field, "template must not contain a drive letter or ':'"})
	}
	if requireNight && !strings.Contains(tmpl, "<n>") {
		ps = append(ps, Problem{SeverityError, field,
			fmt.Sprintf("%s template must contain the night segment token <n>", scope)})
	}
	for _, tok := range tokenPattern.FindAllString(tmpl, -1) {
		if !canonicalTokens[tok] {
			ps = append(ps, Problem{SeverityError, field,
				fmt.Sprintf("%s is not a valid variable name", tok)})
			continue
		}
		if mosaicOnlyTokens[tok] && scope != scopeMosaic {
			ps = append(ps, Problem{SeverityError, field,
				fmt.Sprintf("%s can only be used in the mosaic template; elsewhere it has nothing to render and the frame is quarantined", tok)})
		}
	}
	if scope == scopeMosaic {
		ps = append(ps, mosaicDistinguishesPanels(field, tmpl)...)
	}

	if worstCasePathLen(root, tmpl) > maxPathBudget {
		ps = append(ps, Problem{SeverityWarning, field,
			fmt.Sprintf("worst-case realized path may exceed %d characters (Windows MAX_PATH)", maxPathBudget)})
	}
	return ps
}

// mosaicDistinguishesPanels refuses a mosaic template that would render the same
// directory for two panels of one target.
//
// It is positional, not a containment test: the union of tokens is taken across
// the segments BEFORE the first one naming <n>, and must include <TARGET>, or
// <PANEL>, or both <PANEL_ROW> and <PANEL_COL>. The union rather than a single
// segment, so ".../<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>" — which tells panels apart
// perfectly — is accepted; the FIRST <n> segment, so a template naming <n> twice
// is unambiguous.
//
// Containment alone would accept ".../NIGHT_<n>/<PANEL>", which is WORSE than
// the flat layout this rule exists to forbid. A flat layout puts ten OBJECT
// values in one night folder and loses nine ordinals, because the rebuild reads
// one header per folder; a panel folder *below* the night folder loses them all,
// because the rebuild stops descending at NIGHT_<n> and skips directory entries,
// so that folder yields no entry at all.
func mosaicDistinguishesPanels(field, tmpl string) []Problem {
	// route.SplitAtNight is the one implementation of "the segments above the
	// night folder": the router renders the same prefix when it reads a folder's
	// night ordinal, and a second walk here would be free to disagree with it.
	before, _, ok := route.SplitAtNight(tmpl)
	// No <n> at all: requireNight has already reported the real fault, and there
	// is no meaningful prefix to judge. A second problem here would only guess.
	if !ok {
		return nil
	}

	tokens := map[string]bool{}
	for _, seg := range before {
		for _, tok := range tokenPattern.FindAllString(seg, -1) {
			tokens[tok] = true
		}
	}
	if tokens["<TARGET>"] || tokens["<PANEL>"] || (tokens["<PANEL_ROW>"] && tokens["<PANEL_COL>"]) {
		return nil
	}
	return []Problem{{SeverityError, field,
		"mosaic template must tell panels apart in a folder ABOVE its <n> segment — add <PANEL> " +
			"(or <TARGET>, or both <PANEL_ROW> and <PANEL_COL>) there. Otherwise every panel shares " +
			"one night folder and their night numbers are lost at the next restart; putting the panel " +
			"below NIGHT_<n> loses them too."}}
}

// worstCasePathLen estimates the longest realized path this template could
// produce under root: literal characters plus a plausible expansion for each
// <TOKEN>, plus a leaf filename.
func worstCasePathLen(root, tmpl string) int {
	n := len(root) + 1 + plausibleFilenameLen + 1 // root + sep + filename + sep
	inToken := false
	for _, r := range tmpl {
		switch {
		case r == '<':
			inToken = true
			n += plausibleSegmentLen
		case r == '>':
			inToken = false
		case !inToken:
			n++
		}
	}
	return n
}

func validateMisc(cfg *model.Config) []Problem {
	var ps []Problem

	if !validDateLayout(cfg.DateFormat) {
		ps = append(ps, Problem{SeverityError, "date_format",
			fmt.Sprintf("%q is not a usable Go date layout", cfg.DateFormat)})
	}
	if cfg.MaxConcurrentTransfers < 1 {
		ps = append(ps, Problem{SeverityError, "max_concurrent_transfers",
			"must be at least 1"})
	}
	if cfg.WebPort < 1 || cfg.WebPort > 65535 {
		ps = append(ps, Problem{SeverityError, "web_port",
			fmt.Sprintf("%d is outside the valid port range 1-65535", cfg.WebPort)})
	}
	if cfg.PreviewConcurrency < 1 {
		ps = append(ps, Problem{SeverityError, "preview_concurrency",
			"must be at least 1"})
	}
	// Floor the cache cap so it can always hold at least one full-resolution preview
	// (a single entry is always admitted; the cap bounds the rest).
	if cfg.PreviewCacheMaxBytes < 256*1024*1024 {
		ps = append(ps, Problem{SeverityError, "preview_cache_max_bytes",
			"must be at least 256 MiB"})
	}
	// A hinted solve's search radius: too small risks missing the true center when
	// the pointing hint is coarse; too large slows the solve and invites a false
	// match. 1–60° brackets the useful range (10° is the default).
	if cfg.PlateSolveRadiusDeg < 1 || cfg.PlateSolveRadiusDeg > 60 {
		ps = append(ps, Problem{SeverityError, "plate_solve_radius_deg",
			fmt.Sprintf("%d is outside the valid range 1-60 degrees", cfg.PlateSolveRadiusDeg)})
	}
	return ps
}

func validateSources(cfg *model.Config) []Problem {
	var ps []Problem

	// Which profile-sourced tokens do the folder templates reference? A rig field
	// they need (OTA, Session) that is blank makes every frame quarantine with a
	// "<TOKEN> rendered empty" — surface that at save time instead of at 3am.
	//
	// Scanned per template rather than over one concatenation, so the message can
	// say WHICH template created the need — and so the mosaic template can be
	// left out entirely while detection is off, when it routes nothing.
	needsOTA := templatesNaming(cfg, "<OTA>")
	needsSession := templatesNaming(cfg, "<SESSION>")

	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		field := fmt.Sprintf("rigs[%s].sources", rig.ID)

		// A managed rig must actually watch something. A source with no folders
		// watches NOTHING (the worker walks each named root; an empty list walks
		// none) — it does not fall back to the whole share — so a blank folder
		// list would silently pull zero frames, the exact failure this tool exists
		// to prevent. Require at least one source with at least one folder.
		if rig.Managed {
			watched := 0
			for _, s := range rig.Sources {
				if strings.TrimSpace(s.Share) == "" {
					ps = append(ps, Problem{SeverityError, field, "a watched share has no name"})
					continue
				}
				roots := 0
				for _, r := range s.Roots {
					if strings.TrimSpace(r) != "" {
						roots++
					}
				}
				if roots == 0 {
					ps = append(ps, Problem{SeverityError, field,
						fmt.Sprintf("share %q watches no folders — add a folder (e.g. Autorun); a blank list pulls nothing", s.Share)})
					continue
				}
				watched++
			}
			if watched == 0 && len(rig.Sources) == 0 {
				ps = append(ps, Problem{SeverityError, field, "a managed rig must watch at least one share and folder"})
			}
			// One problem per rig per field, listing every template that needs the
			// token: the SPA routes field errors by Field, so a second problem on
			// the same one would render as a duplicate.
			if len(needsOTA) > 0 && strings.TrimSpace(rig.OTA) == "" {
				phrase, verb := templatesPhrase(needsOTA)
				ps = append(ps, Problem{SeverityError, fmt.Sprintf("rigs[%s].ota", rig.ID),
					fmt.Sprintf("OTA is required — %s %s <OTA>, and a blank value sends every frame to unclassified. Either set an OTA name here, or remove <OTA> from %s in Settings.", phrase, verb, phrase)})
			}
			if len(needsSession) > 0 && strings.TrimSpace(rig.Session) == "" {
				phrase, verb := templatesPhrase(needsSession)
				ps = append(ps, Problem{SeverityError, fmt.Sprintf("rigs[%s].session", rig.ID),
					fmt.Sprintf("Session is required — %s %s <SESSION>, and a blank value sends every frame to unclassified. Either set a Session name here, or remove <SESSION> from %s in Settings.", phrase, verb, phrase)})
			}
		}

		hasUdisk, hasEMMC := false, false
		for _, s := range rig.Sources {
			share := strings.ToLower(s.Share)
			if strings.Contains(share, "udisk") {
				hasUdisk = true
			}
			if strings.Contains(share, "emmc") {
				hasEMMC = true
			}
		}
		if hasUdisk && !hasEMMC {
			ps = append(ps, Problem{SeverityWarning,
				fmt.Sprintf("rigs[%s].sources", cfg.Rigs[i].ID),
				"watches a USB (Udisk) share but not EMMC; a mid-session USB dropout fails over to eMMC (H12) and those frames would never be pulled"})
		}
	}
	return ps
}

// templatesNaming returns, in a fixed order, the names of the folder templates
// containing tok. The mosaic template is included only while detection is on:
// off, it routes nothing, so a rig field it alone would need is not actually
// required and blocking the save over it would be a false alarm.
func templatesNaming(cfg *model.Config, tok string) []string {
	var names []string
	if strings.Contains(cfg.Templates.Light, tok) {
		names = append(names, "light")
	}
	for _, t := range model.CanonicalTypes {
		if t == model.TypeLight {
			continue
		}
		if strings.Contains(cfg.Templates.Cal[t], tok) {
			names = append(names, strings.ToLower(string(t)))
		}
	}
	if cfg.MosaicDetection && strings.Contains(cfg.Templates.Mosaic, tok) {
		names = append(names, "mosaic")
	}
	return names
}

// templatesPhrase renders a template-name list for a message — "your light
// template", "your light and mosaic templates" — with the verb that agrees with
// it. Callers only reach it with a non-empty list.
func templatesPhrase(names []string) (phrase, verb string) {
	switch len(names) {
	case 1:
		return "your " + names[0] + " template", "uses"
	case 2:
		return "your " + names[0] + " and " + names[1] + " templates", "use"
	default:
		return "your " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " templates", "use"
	}
}

func validateHosts(cfg *model.Config, vol VolumeResolver) []Problem {
	var ps []Problem
	resolver, _ := vol.(HostResolver)

	seen := make(map[string]string) // address key -> first rig id
	for i := range cfg.Rigs {
		host := cfg.Rigs[i].Host
		if strings.TrimSpace(host) == "" {
			continue
		}
		key := strings.ToLower(host)
		if resolver != nil {
			if addr, err := resolver.Resolve(host); err == nil {
				key = strings.ToLower(addr)
			}
		}
		if first, ok := seen[key]; ok {
			ps = append(ps, Problem{SeverityWarning,
				fmt.Sprintf("rigs[%s].host", cfg.Rigs[i].ID),
				fmt.Sprintf("resolves to the same address as rig %q", first)})
			continue
		}
		seen[key] = cfg.Rigs[i].ID
	}
	return ps
}

// validDateLayout reports whether s is a usable Go time layout: formatting two
// distinct instants must yield distinct strings (so it carries at least one date
// token, not just literal text) and the result must parse back with the same
// layout.
func validDateLayout(s string) bool {
	if s == "" {
		return false
	}
	t1 := time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC)
	t2 := time.Date(2007, time.November, 23, 1, 2, 3, 0, time.UTC)
	s1, s2 := t1.Format(s), t2.Format(s)
	if s1 == s2 {
		return false
	}
	if _, err := time.Parse(s, s1); err != nil {
		return false
	}
	return true
}

func normExt(e string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
}

func normExtSet(exts []string) map[string]bool {
	set := make(map[string]bool, len(exts))
	for _, e := range exts {
		set[normExt(e)] = true
	}
	return set
}

// A duration typed without a unit — "60" instead of "5m" — fails inside
// Duration.UnmarshalJSON during json.Decode, before Validate ever runs, so its
// field path is lost to the generic "invalid body" error. LocalizeDecodeError and
// LocalizeRigDecodeError re-scan the raw body on the decode-failure path and
// rebuild a per-field Problem for the offending duration, using the same Field
// form the validator emits, so the SPA can light it up inline (design
// architecture §8.8).
const (
	// durationUnitHint is shown when a duration string parsed with no unit.
	durationUnitHint = "Enter a duration with a unit — for example 60s, 5m, or 24h."
	// durationTypeHint is shown when a duration field held a non-string value
	// (a number or object) — a distinct, more structural mistake than a bad unit.
	durationTypeHint = `This should be a duration in quotes, like "5m" or "24h".`
)

// defaultsDurationFields are the RigDefaults durations a form can edit, by their
// JSON key under "defaults". The explicit allowlist (not reflection) keeps the
// walk to fields a UI actually produces; TestDurationFieldsCategorized forces it
// to stay in sync with model.Config.
var defaultsDurationFields = []string{
	"poll_interval", "retry_interval", "min_file_age", "grace", "reconcile_settle",
}

// rigDurationFields are the per-rig override durations a rig editor can edit, by
// their JSON key on a RigProfile.
var rigDurationFields = []string{
	"grace", "poll_interval", "retry_interval", "min_file_age", "reconcile_settle",
}

// LocalizeDecodeError inspects a Config-shaped JSON body that failed to decode and
// returns a per-field Problem for every form-editable duration whose raw value is
// a bad duration string (unit hint) or a non-string (type hint). It returns nil
// when the body is not structurally JSON, or when no form duration is at fault —
// the caller then falls back to the generic banner. decErr is unused but
// kept in the signature so the contract can key on the decode error later.
func LocalizeDecodeError(raw []byte, decErr error) []Problem {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}

	var ps []Problem
	ps = append(ps, durationProblem("missing_display_period", root["missing_display_period"])...)

	if d := root["defaults"]; len(d) > 0 {
		var defaults map[string]json.RawMessage
		if json.Unmarshal(d, &defaults) == nil {
			for _, name := range defaultsDurationFields {
				ps = append(ps, durationProblem("defaults."+name, defaults[name])...)
			}
		}
	}

	if rj := root["rigs"]; len(rj) > 0 {
		var rigs []json.RawMessage
		if json.Unmarshal(rj, &rigs) == nil {
			seen := map[string]bool{}
			for _, elem := range rigs {
				var fields map[string]json.RawMessage
				if json.Unmarshal(elem, &fields) != nil {
					continue
				}
				id := rawStringField(fields["id"])
				// Skip an element with a missing or duplicate id: its Field path
				// would be ambiguous, so the generic banner is the honest fallback.
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				for _, name := range rigDurationFields {
					ps = append(ps, durationProblem(fmt.Sprintf("rigs[%s].%s", id, name), fields[name])...)
				}
			}
		}
	}

	if len(ps) == 0 {
		return nil
	}
	return ps
}

// LocalizeRigDecodeError is LocalizeDecodeError's sibling for the rig endpoints,
// which decode a bare model.RigProfile (its durations are top-level, with no
// rigs[] wrapper). It prefixes each field with rigs[<rigID>]. so a bad duration in
// a per-rig override renders inline in the Devices editor. rigID comes from the
// URL for an update and from the body for a create; without one, no field can be
// named, so it returns nil.
func LocalizeRigDecodeError(raw []byte, rigID string) []Problem {
	if strings.TrimSpace(rigID) == "" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	var ps []Problem
	for _, name := range rigDurationFields {
		ps = append(ps, durationProblem(fmt.Sprintf("rigs[%s].%s", rigID, name), fields[name])...)
	}
	if len(ps) == 0 {
		return nil
	}
	return ps
}

// durationProblem returns a Problem for a single duration field's raw JSON value,
// or nil when the value is absent or a valid duration string. A present-but-bad
// string yields the unit hint; a present non-string yields the type hint.
func durationProblem(field string, rawVal json.RawMessage) []Problem {
	if len(rawVal) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(rawVal, &s) == nil {
		if _, err := time.ParseDuration(s); err != nil {
			return []Problem{{SeverityError, field, durationUnitHint}}
		}
		return nil
	}
	return []Problem{{SeverityError, field, durationTypeHint}}
}

// rawStringField returns the string a raw JSON value holds, or "" if it is not a
// string (or absent).
func rawStringField(rawVal json.RawMessage) string {
	var s string
	if json.Unmarshal(rawVal, &s) == nil {
		return s
	}
	return ""
}
