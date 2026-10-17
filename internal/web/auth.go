package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"golang.org/x/crypto/bcrypt"
)

const (
	// sessionCookie is signed HttpOnly + SameSite=Strict; xsrfCookie is the
	// readable double-submit token the SPA echoes in X-CSRF-Token.
	sessionCookie = "session"
	xsrfCookie    = "XSRF-TOKEN"
	csrfHeader    = "X-CSRF-Token"
)

// sessionClaims is the signed cookie payload. IssuedAt anchors the absolute
// expiry (a captured cookie can't outlive it); Seen anchors the idle timeout and
// slides forward on each authenticated request (the middleware re-issues the
// cookie), so an idle session lapses while an active one persists to the absolute
// cap. XSRF binds the double-submit token to the session so a stale token from a
// prior login can't ride a new session.
type sessionClaims struct {
	IssuedAt int64  `json:"iat"`
	Seen     int64  `json:"seen"`
	XSRF     string `json:"xsrf"`
}

// signSession renders claims as base64url(payload).base64url(hmac), the HMAC
// taken over the payload segment with the config session secret.
func signSession(secret string, c sessionClaims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	sig := sign([]byte(secret), p)
	return p + "." + sig, nil
}

// verifySession validates a cookie value against the secret and the absolute/idle
// windows, returning the claims on success.
func verifySession(secret, value string, idle, maxAge time.Duration, now time.Time) (sessionClaims, error) {
	if secret == "" {
		return sessionClaims{}, errors.New("no session secret")
	}
	p, sig, ok := strings.Cut(value, ".")
	if !ok {
		return sessionClaims{}, errors.New("malformed session cookie")
	}
	if !hmac.Equal([]byte(sig), []byte(sign([]byte(secret), p))) {
		return sessionClaims{}, errors.New("bad session signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return sessionClaims{}, err
	}
	var c sessionClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return sessionClaims{}, err
	}
	nowUnix := now.Unix()
	if maxAge > 0 && nowUnix > c.IssuedAt+int64(maxAge.Seconds()) {
		return sessionClaims{}, errors.New("session expired (absolute)")
	}
	if idle > 0 && nowUnix > c.Seen+int64(idle.Seconds()) {
		return sessionClaims{}, errors.New("session expired (idle)")
	}
	return c, nil
}

// sign returns base64url(HMAC-SHA256(secret, msg)).
func sign(secret []byte, msg string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// setSessionCookies writes the signed session cookie and the readable XSRF
// cookie. Secure is intentionally unset (plain HTTP on the trusted LAN, design
// architecture §8); SameSite=Strict + HttpOnly protect the session cookie, and
// the XSRF cookie is deliberately readable so the SPA can echo it.
func (s *Server) setSessionCookies(w http.ResponseWriter, cfg *model.Config, c sessionClaims) error {
	value, err := signSession(cfg.SessionSecret, c)
	if err != nil {
		return err
	}
	maxAge := int(cfg.Web.SessionMaxAge.D().Seconds())
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     xsrfCookie,
		Value:    c.XSRF,
		Path:     "/",
		HttpOnly: false, // the SPA must read it to set the X-CSRF-Token header
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
	return nil
}

// clearSessionCookies expires both cookies (logout / secret-rotation invalidate).
func clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, xsrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: name == sessionCookie, SameSite: http.SameSiteStrictMode,
		})
	}
}

// session validates the request's session cookie against the current config and
// returns the claims. The caller decides the response on error.
func (s *Server) session(r *http.Request) (sessionClaims, error) {
	cfg := s.currentConfig()
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return sessionClaims{}, errors.New("no session cookie")
	}
	return verifySession(cfg.SessionSecret, ck.Value,
		cfg.Web.SessionIdleTimeout.D(), cfg.Web.SessionMaxAge.D(), s.now())
}

// handleLogin verifies the shared password and, on success, issues the session +
// XSRF cookies. It is rate-limited (per-source backoff + global cap) and
// Host/Origin-checked by its guard, but exempt from the CSRF-token check since no
// session exists yet.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if cfg.PasswordHash == "" {
		writeError(w, http.StatusConflict, "no password set; complete setup first")
		return
	}

	source := hostOnly(r.RemoteAddr)
	if retry, ok := s.limiter.check(source, s.now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many login attempts; slow down")
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte(body.Password)); err != nil {
		s.limiter.failure(source, s.now(), cfg.Web.LoginBackoffBase.D(), cfg.Web.LoginBackoffCap.D(), cfg.Web.LoginGlobalCap)
		s.log.Warn("web: failed login", "source", source)
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}

	s.limiter.success(source)
	now := s.now()
	claims := sessionClaims{IssuedAt: now.Unix(), Seen: now.Unix(), XSRF: genToken()}
	if err := s.setSessionCookies(w, cfg, claims); err != nil {
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}
	s.log.Info("web: login", "source", source)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleLogout clears this browser's session cookies and rotates the session
// secret so every outstanding cookie (here and on any other device) stops
// verifying. It is guarded like any mutation (session + CSRF + Host/Origin), so a
// cross-site page cannot force a logout. The local clear always happens; a failed
// rotation is logged but does not block the caller's own logout.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookies(w)
	if err := s.rotateSessionSecret(); err != nil {
		s.log.Error("web: session secret rotation on logout failed", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// --- guards --------------------------------------------------------------------

// guardRead requires a valid Host and session. On success it slides the session's
// idle window by re-issuing the cookie.
func (s *Server) guardRead(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkHost(w, r) {
			return
		}
		if !s.requireSession(w, r) {
			return
		}
		h(w, r)
	}
}

// guardWrite requires Host, Origin, a valid session, and a matching CSRF token —
// the full mutation defence (design architecture §8).
func (s *Server) guardWrite(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkHost(w, r) {
			return
		}
		if !s.checkOrigin(w, r) {
			return
		}
		claims, ok := s.sessionOr403(w, r)
		if !ok {
			return
		}
		if !s.checkCSRF(w, r, claims) {
			return
		}
		h(w, r)
	}
}

// guardLogin protects the login endpoint: Host + Origin, but no session or CSRF
// token (there is no session yet). Rate-limiting is applied inside the handler.
func (s *Server) guardLogin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkHost(w, r) {
			return
		}
		if !s.checkOrigin(w, r) {
			return
		}
		h(w, r)
	}
}

// requireSession validates the session and slides its idle window; it writes 401
// and returns false on failure.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) bool {
	_, ok := s.sessionOr403(w, r)
	return ok
}

// sessionOr403 validates the session, re-issues the sliding cookie, and returns
// the claims. On failure it writes 401 and returns ok=false.
func (s *Server) sessionOr403(w http.ResponseWriter, r *http.Request) (sessionClaims, bool) {
	claims, err := s.session(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return sessionClaims{}, false
	}
	// Slide the idle window forward (keeps XSRF binding stable).
	refreshed := claims
	refreshed.Seen = s.now().Unix()
	if err := s.setSessionCookies(w, s.currentConfig(), refreshed); err != nil {
		s.log.Warn("web: could not refresh session cookie", "err", err)
	}
	return claims, true
}

// genToken returns a URL-safe 256-bit random token (setup token, XSRF token).
func genToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// genSecret returns a fresh base64 session secret, used if a config somehow
// reaches PUT with an empty secret post-setup.
func genSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
