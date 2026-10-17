package web

import (
	"errors"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
	"golang.org/x/crypto/bcrypt"
)

// errSetupHandled signals that the setup config-write closure already wrote the
// HTTP response, so the handler must stop without writing again.
var errSetupHandled = errors.New("setup response already written")

// setupTokenHeader carries the one-time token on the setup POST.
const setupTokenHeader = "X-Setup-Token"

// Machine-readable refusal codes for the setup route, so the SPA can tell a
// wrong token from a window that has already closed without matching prose.
const (
	codeSetupTokenRequired = "setup_token_required"
	codeSetupComplete      = "setup_complete"
)

// handleSetupStatus reports whether the first-run wizard is needed, plus — only
// while it is — the platform category the wizard words its "where do I find the
// token" help from, and the operating system it picks default folders for (the
// wizard builds those paths itself, so no path crosses this endpoint).
//
// It never returns the token, the token file's contents, or an absolute
// filesystem path. This is the one unauthenticated surface whose job is helping
// operators find the credential, which makes it exactly where a well-meaning
// change would collapse the scheme; and on non-Windows the data directory sits
// under $HOME, so a path would disclose the OS username and install layout to
// any caller.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needed := s.currentConfig().PasswordHash == ""
	// The Host allowlist is bypassed here only while the first-run window is open.
	// Pre-setup the list is loopback-only, so an operator reaching a headless NAS
	// by name would fail this probe and the SPA would never route to the wizard —
	// it would just render broken, because the static bundle is unguarded. Once a
	// password exists the allowlist applies again, unchanged.
	if !needed && !s.checkHost(w, r) {
		return
	}
	body := map[string]any{"needed": needed}
	if needed {
		body["platform"] = PlatformCategory()
		body["os"] = runtime.GOOS
	}
	writeJSON(w, http.StatusOK, body)
}

// guardSetup gates the takeover-sensitive setup route. Possession of the
// one-time token is necessary and sufficient to complete setup, and no client is
// exempt — not loopback, not the console, not the operator's own machine.
//
// The token is 256 bits from crypto/rand, and its sufficiency rests on secrecy
// rather than on "a cross-site page cannot set a custom header", which fails
// under DNS rebinding: no route emits an Access-Control-Allow-* header, so no
// foreign document can read a response that would disclose it.
//
// checkOrigin leaves this guard entirely. The drive-by it defended against — a
// cross-site page in a browser on the console becoming the local client — is
// defeated by the token requirement instead, and more completely. So is the
// break that killed the earlier console exemption: a default-configured nginx
// rewrites Host to localhost, so a request from the internet can present a
// loopback peer, a loopback Host, and a loopback Origin at once. Every signal
// available here is attacker-controlled once a loopback-dialling proxy exists,
// and ANDing forgeable signals yields a forgeable signal.
//
// Failed attempts are deliberately not rate-limited: guessing 256 bits is not a
// threat a throttle reduces, and under container NAT every client shares one
// source bucket, so a per-source limiter would let anyone reaching the port lock
// the operator out of their own first-run window. The logging of failures is
// rate-limited instead.
func (s *Server) guardSetup(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.currentConfig().PasswordHash != "" && !s.checkHost(w, r) {
			return // window closed: the allowlist applies again, exactly as before
		}
		if s.currentConfig().PasswordHash != "" {
			writeErrorCode(w, http.StatusForbidden, codeSetupComplete, "setup is already complete")
			return
		}
		if !s.validSetupToken(r) {
			s.logSetupRejection(r)
			writeErrorCode(w, http.StatusForbidden, codeSetupTokenRequired,
				setupTokenMessage(r.Header.Get(setupTokenHeader) != ""))
			return
		}
		h(w, r)
	}
}

// setupTokenMessage words the refusal differently for a wrong token and for no
// token at all: one is a correction, the other is an instruction.
func setupTokenMessage(offered bool) string {
	if offered {
		return "that setup token doesn't match the one from the service log; " +
			"if Commander has restarted since you copied it, check again for the current one"
	}
	return "finishing setup needs the one-time token from the service log"
}

// validSetupToken reports whether the request carries the live token. The nil
// check is load-bearing: the atomic loads nil both before the mint and after the
// window closes, and a nil-deref there would be a panic on every setup attempt
// against a configured service.
func (s *Server) validSetupToken(r *http.Request) bool {
	tok := s.setupToken.Load()
	return tok != nil && *tok != "" && constantEqual(r.Header.Get(setupTokenHeader), *tok)
}

// setupRejectionInterval is the minimum gap between two WARNs about the same
// peer, and the fallback global gap once the peer table is full.
const setupRejectionInterval = time.Minute

// setupRejectionPeers bounds the per-peer table. It is keyed by unauthenticated
// input, so it must not grow with the number of distinct sources an attacker
// chooses to present.
const setupRejectionPeers = 64

// setupRejectionLog collapses repeated setup refusals to one WARN per peer per
// minute. Without it an attacker can fill the disk, or evict the very startup
// line the operator is reading the token from — which is the failure that would
// matter, since the token has no other in-band channel on a headless host.
type setupRejectionLog struct {
	mu     sync.Mutex
	peers  map[string]time.Time
	global time.Time
}

// allow reports whether a refusal from peer should be logged at now.
func (l *setupRejectionLog) allow(peer string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.peers == nil {
		l.peers = make(map[string]time.Time, setupRejectionPeers)
	}
	if last, ok := l.peers[peer]; ok {
		if now.Sub(last) < setupRejectionInterval {
			return false
		}
		l.peers[peer] = now
		return true
	}
	if len(l.peers) >= setupRejectionPeers {
		// Evict entries older than the interval first: they can no longer suppress
		// anything, so keeping them would let a burst of distinct sources jam the
		// table permanently.
		for p, t := range l.peers {
			if now.Sub(t) >= setupRejectionInterval {
				delete(l.peers, p)
			}
		}
	}
	if len(l.peers) >= setupRejectionPeers {
		// Still full: fall back to one WARN per minute globally rather than growing.
		if now.Sub(l.global) < setupRejectionInterval {
			return false
		}
		l.global = now
		return true
	}
	l.peers[peer] = now
	return true
}

// logSetupRejection records a refused setup attempt, rate-limited per peer.
//
// It logs the peer and the Host only. The attempted token value is NEVER
// logged: it is attacker-supplied, it would sit in the same log the real token
// is read from, and a near-miss in a log an operator might paste into a bug
// report is a credential leak.
func (s *Server) logSetupRejection(r *http.Request) {
	if !s.setupRejects.allow(hostOnly(r.RemoteAddr), s.now()) {
		return
	}
	s.log.Warn("web: setup refused, no valid one-time token",
		"remote", r.RemoteAddr, "host", r.Host)
}

// setupRequest is the wizard body: the shared password (required), an optional
// full config to seed (defaults are used when it is absent), and the addresses
// the operator will reach Commander at.
//
// AllowedHosts is a dedicated field rather than a value carried inside Config,
// because sending any config at all flips handleSetup's volume resolver on and
// the wizard's placeholder roots would then fail validation on a first run.
// The config is decoded through configPut for the same reason handlePutConfig
// is: a client older than the server omits mosaic_detection and templates.mosaic,
// and an omitted bool decodes as false.
type setupRequest struct {
	Password     string     `json:"password"`
	Config       *configPut `json:"config,omitempty"`
	AllowedHosts []string   `json:"allowed_hosts,omitempty"`
}

// handleSetup completes the first run: it hashes the password, generates the
// session secret, assembles the Host allowlist, saves, applies, swaps the live
// pointer, closes the takeover window, and logs the operator in.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Password) == "" {
		writeError(w, http.StatusBadRequest, "a password is required")
		return
	}

	var cfg *model.Config
	resolver := s.volumeResolver()
	if req.Config != nil {
		cfg = req.Config.merge(s.currentConfig())
	} else {
		cfg = config.Defaults()
		// The wizard seeds defaults with placeholder roots (e.g. D:\Astro\Lights)
		// the operator has not chosen yet — do not fail first-run setup because that
		// placeholder drive is absent on this machine. The operator's real roots are
		// volume-validated by the subsequent config PUT.
		resolver = nil
	}

	cost := cfg.Web.BcryptCost
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = 12
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), cost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not hash password")
		return
	}
	cfg.PasswordHash = string(hash)
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = genSecret()
	}

	// Serialize the config write behind the shared guard, like every other writer.
	// The first-run placeholder roots keep using a nil volume resolver on purpose;
	// the plain-mutex guard preserves that. errSetupHandled signals the response was
	// already written inside fn.
	var seededHost string
	var seededIsIP bool
	if err := s.deps.Guard.Do(func() error {
		// Re-take both of guardSetup's checks under the lock. The guard ran before
		// bcrypt at cost 12, a deliberately wide window: two concurrent POSTs (a
		// double-click, a retried request, an attacker racing the operator with a
		// leaked token) would otherwise both pass, both save, and the loser's
		// password would silently replace the winner's.
		if s.currentConfig().PasswordHash != "" {
			writeErrorCode(w, http.StatusForbidden, codeSetupComplete, "setup is already complete")
			return errSetupHandled
		}
		if !s.validSetupToken(r) {
			writeErrorCode(w, http.StatusForbidden, codeSetupTokenRequired, setupTokenMessage(true))
			return errSetupHandled
		}

		// The allowlist assembly, in the order the design fixes it so "which one
		// wins" is not inferred from reading the code:
		//
		//  1. validate the raw, pre-normalization submitted entries;
		//  2. union — the operator's addresses, then any carried in a submitted
		//     config, then the request's own origin, then this machine's hostname;
		//  3. normalize and de-duplicate last, over the whole union.
		//
		// The seeded origin is validated separately (wildcard/scheme/whitespace only)
		// and os.Hostname() is exempt entirely: it is the machine's own name, not
		// operator input.
		submitted := append(append([]string{}, req.AllowedHosts...), cfg.Web.AllowedHosts...)
		if problems := config.ValidateSubmittedHosts(submitted, s.currentConfig().Web.AllowedHosts); config.HasErrors(problems) {
			s.log.Warn("web: setup rejected invalid addresses", "problems", problems)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid configuration", "problems": problems,
			})
			return errSetupHandled
		}
		cfg.Web.AllowedHosts = submitted
		seededHost, seededIsIP = seedRequestOrigin(cfg, r)
		seedHostname(cfg)
		// Canonicalize a caller-supplied config the same way saveConfigLocked
		// does: without it, this path is safe only while the shipped wizard posts no
		// config at all, and a "\"-authored template would save uncanonically
		// and a blank mosaic template would fail validation instead of taking the
		// default.
		config.NormalizeTemplates(cfg)
		config.NormalizeAllowedHosts(cfg)

		if problems := config.Validate(cfg, resolver); config.HasErrors(problems) {
			s.log.Warn("web: setup rejected invalid configuration", "problems", problems)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid configuration", "problems": problems,
			})
			return errSetupHandled
		}
		if err := config.Save(s.deps.ConfigPath, cfg); err != nil {
			s.log.Error("web: setup save failed", "err", err)
			writeError(w, http.StatusInternalServerError, "could not save configuration")
			return errSetupHandled
		}
		s.deps.Supervisor.ApplyConfig(cfg)
		s.deps.Config.Store(cfg)

		// Clear the token only after the write has succeeded. Clearing earlier would
		// mean a rejected config or a failed save (disk full, ACL) destroys the
		// credential while leaving no password set, stranding the operator in a
		// window they cannot re-enter without restarting the service.
		s.setupToken.Store(nil)
		s.removeSetupTokenFile()
		return nil
	}); err != nil {
		return // response already written inside the guard
	}

	now := s.now()
	claims := sessionClaims{IssuedAt: now.Unix(), Seen: now.Unix(), XSRF: genToken()}
	if err := s.setSessionCookies(w, cfg, claims); err != nil {
		writeError(w, http.StatusInternalServerError, "setup saved but session could not start")
		return
	}
	s.log.Info("web: setup complete", "seeded_host", seededHost)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "config": config.Redact(cfg),
		"seeded_host": seededHost, "seeded_host_is_ip": seededIsIP,
	})
}

// seedRequestOrigin allowlists the origin the operator's browser will actually
// send, so the follow-on roots check, the config PUT, and every later login pass
// checkOrigin from that same browser. It returns what was seeded and whether
// that value is an IP literal, which the wizard shows as a fragility note.
//
// Origin is what checkOrigin compares; Host is only the fallback for a client
// that sent neither Origin nor Referer. Seeding hostOnly(r.Host) instead would
// allowlist the wrong value wherever a proxy rewrites Host — storing "localhost",
// already allowed and therefore a no-op, while the operator's real browser origin
// stays blocked and the wizard dies on its next call.
func seedRequestOrigin(cfg *model.Config, r *http.Request) (seeded string, isIP bool) {
	origin, _ := originHost(r)
	// Fall back whenever the Origin host is empty, regardless of originHost's
	// present flag: an unparseable or "null" Origin (a sandboxed iframe, a file://
	// page) reports ("", true), which a naive present check would take as usable.
	for _, candidate := range []string{origin, hostOnly(r.Host)} {
		host := config.NormalizeHostEntry(candidate)
		// Origin is free text a non-browser client sets to anything, so the seeded
		// value still passes the wildcard, scheme, and whitespace rules; only the
		// hostname-shape check is waived, because this value demonstrably reached
		// the server from the operator's own browser. On failure, fall back; then
		// seed nothing.
		if !config.AcceptableSeededHost(host) {
			continue
		}
		for _, existing := range cfg.Web.AllowedHosts {
			if config.NormalizeHostEntry(existing) == host {
				_, err := netip.ParseAddr(host)
				return host, err == nil
			}
		}
		cfg.Web.AllowedHosts = append(cfg.Web.AllowedHosts, host)
		_, err := netip.ParseAddr(host)
		return host, err == nil
	}
	return "", false
}

// seedHostname adds this machine's hostname to the allowlist (seeded as a name,
// not a raw IP, so DHCP churn is harmless — design architecture §8). Idempotent.
func seedHostname(cfg *model.Config) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return
	}
	host = strings.ToLower(host)
	for _, h := range cfg.Web.AllowedHosts {
		if strings.ToLower(h) == host {
			return
		}
	}
	cfg.Web.AllowedHosts = append(cfg.Web.AllowedHosts, host)
}
