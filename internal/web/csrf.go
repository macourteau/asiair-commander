package web

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

// checkHost enforces the Host allowlist (design architecture §8): every
// request's Host must be a configured name (loopback always permitted). This is
// the anti-DNS-rebinding backstop — a forged Host from a rebinding page is
// rejected here even when the browser treats the connection as same-origin.
// Writes 403 and returns false on failure.
func (s *Server) checkHost(w http.ResponseWriter, r *http.Request) bool {
	if isPrivateHostIP(r.Host) || s.allowedHosts(s.currentConfig())[strings.ToLower(hostOnly(r.Host))] {
		return true
	}
	s.log.Warn("web: host not allowed", "host", r.Host, "path", r.URL.Path)
	writeError(w, http.StatusForbidden, fmt.Sprintf("host %q not allowed; add it to web.allowed_hosts in config.yaml", r.Host))
	return false
}

// checkOrigin enforces that a mutating request's Origin (falling back to Referer)
// resolves to an allowed host (design architecture §8). A missing
// Origin/Referer is rejected: a browser always sends Origin on a cross-origin —
// and modern browsers on a same-origin — mutating fetch, so its absence signals
// a non-browser or forged caller. Writes 403 and returns false on failure.
func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	host, present := originHost(r)
	if !present {
		s.log.Warn("web: missing Origin/Referer on mutation", "path", r.URL.Path)
		writeError(w, http.StatusForbidden, "missing Origin/Referer")
		return false
	}
	if s.allowedHosts(s.currentConfig())[host] {
		return true
	}
	s.log.Warn("web: cross-origin mutation rejected", "origin_host", host, "path", r.URL.Path)
	writeError(w, http.StatusForbidden, "cross-origin request rejected")
	return false
}

// checkCSRF enforces the double-submit token (design architecture §8): the
// X-CSRF-Token header must equal the XSRF-TOKEN cookie AND the token bound into
// the session, all non-empty, compared in constant time. Binding to the session as
// well as the cookie stops a stale token from an earlier login riding a re-issued
// session. Writes 403 and returns false on failure.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request, claims sessionClaims) bool {
	header := r.Header.Get(csrfHeader)
	cookie, err := r.Cookie(xsrfCookie)
	if err != nil || header == "" || cookie.Value == "" {
		writeError(w, http.StatusForbidden, "missing CSRF token")
		return false
	}
	if !constantEqual(header, cookie.Value) || !constantEqual(header, claims.XSRF) {
		s.log.Warn("web: CSRF token mismatch", "path", r.URL.Path)
		writeError(w, http.StatusForbidden, "CSRF token mismatch")
		return false
	}
	return true
}

func constantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
