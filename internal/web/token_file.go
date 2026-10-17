package web

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/macourteau/asiair-commander/internal/config"
)

// SetupTokenFileName is the machine-readable channel for the one-time setup
// token, written inside the data directory. Filesystem permissions are the
// operating system's real "you are local and privileged" check — the one signal
// the HTTP layer cannot forge — so a token in a restricted file is how a local
// process proves local-ness without the guard having to trust an address.
const SetupTokenFileName = "setup-token"

// setupTokenPath is the token file's absolute path, or "" when the file channel
// is disabled. An empty data directory disables it entirely — no write, no
// removal, no warning — because filepath.Join("", SetupTokenFileName) is a bare
// relative name, and without this every test constructing a zero-value Deps
// would create and delete that file in the package working directory on every
// server construction.
func (s *Server) setupTokenPath() string {
	if s.deps.DataDir == "" {
		return ""
	}
	return filepath.Join(s.deps.DataDir, SetupTokenFileName)
}

// WriteSetupTokenFile records the bound URL and the live token as two lines:
//
//	http://localhost:8686/
//	<token>
//
// Two lines rather than JSON because the only consumer is the Windows installer,
// which has no JSON parser any more than it has a YAML one — two reads and
// nothing else. Each line is terminated, including the token's, and the reader
// trims: an untrimmed "\r\n" spliced into "<url>#setup_token=<token>" produces a
// silently broken launch.
//
// The URL is carried rather than reconstructed because it cannot be re-derived
// from the outside: the bind falls back to the default port and then to an
// ephemeral one. The scheme is always http — the hand-off deliberately avoids the
// HTTPS port so first-run setup never opens on a certificate warning.
//
// It is called after the listener binds, not at mint, because no port exists
// yet at construction. A nil error with nothing written is the correct outcome
// when the channel is disabled or the window has already closed.
func (s *Server) WriteSetupTokenFile(url string) error {
	path := s.setupTokenPath()
	if path == "" {
		return nil
	}
	tok := s.SetupToken()
	if tok == "" {
		return nil // window closed: there is nothing to hand off
	}
	return config.WriteFileRestricted(path, []byte(url+"\n"+tok+"\n"))
}

// removeSetupTokenFile deletes the token file, best-effort. It runs at startup
// before minting — so the file's existence proves the running process wrote it —
// and again inside the config guard once setup completes, alongside clearing the
// in-memory token, so the two never disagree.
func (s *Server) removeSetupTokenFile() {
	path := s.setupTokenPath()
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("web: could not remove the setup token file", "path", path, "err", err)
	}
}
