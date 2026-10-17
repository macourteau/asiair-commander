// Package config owns the YAML config-file lifecycle for ASIAIR Commander: load,
// atomic save (with a .bak of the prior version), forward migration, validation,
// secret redaction for the REST API, and platform secret protection (Windows
// DPAPI + restrictive ACLs, best-effort chmod elsewhere). The in-memory form is
// model.Config; this package performs the I/O and the transforms around it
// (design architecture §7, design architecture §8).
//
// Secret handling. model.Config.SessionSecret is held in memory as its plaintext
// form. On disk it is stored as base64(wrapSecret(plaintext)): on Windows
// wrapSecret is DPAPI (machine scope), elsewhere it is the identity, so the
// stored value round-trips on every platform. The base64 layer is applied
// uniformly here so config.go carries no build-tag branch; the platform seam is
// the wrap/unwrap pair and restrictFile (see secret_windows.go / secret_other.go).
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"gopkg.in/yaml.v3"
)

// ErrNotFound is returned by Load when the config file does not exist. The caller
// treats this as a first run and serves the setup wizard (design architecture
// §7).
var ErrNotFound = errors.New("config file not found")

// ErrSchemaTooNew is returned by Load when the stored schema_version is greater
// than this binary understands. An older binary refuses to touch a newer config
// rather than round-tripping it lossily (design architecture §7).
var ErrSchemaTooNew = errors.New("config written by a newer version")

// Load reads and unmarshals the YAML config at path into a model.Config.
//
// It returns ErrNotFound if the file is absent (first run). A stored
// schema_version newer than model.MaxSchemaVersion yields ErrSchemaTooNew. A
// stored schema_version older than model.CurrentSchemaVersion is forward-migrated
// in memory after a fresh .bak of the on-disk file is written; the caller
// persists the migrated form with Save. model.CurrentSchemaVersion and
// model.RemotesSchemaVersion are both read as they are: the latter only marks a
// config holding a remote, so neither is migrated and neither refreshes the
// .bak. The DPAPI-wrapped session_secret and remote pairing secrets are
// unwrapped after load.
func Load(path string) (*model.Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		slog.Info("config not found, first run", "path", path)
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg model.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	slog.Debug("config loaded", "path", path, "schema_version", cfg.SchemaVersion)

	if cfg.SchemaVersion > model.MaxSchemaVersion {
		slog.Error("config schema too new",
			"path", path, "stored", cfg.SchemaVersion, "supported", model.MaxSchemaVersion)
		return nil, fmt.Errorf("%w: stored schema_version %d > supported %d",
			ErrSchemaTooNew, cfg.SchemaVersion, model.MaxSchemaVersion)
	}

	if cfg.SchemaVersion < model.CurrentSchemaVersion {
		slog.Info("migrating config forward",
			"path", path, "from", cfg.SchemaVersion, "to", model.CurrentSchemaVersion)
		// Refresh the .bak with the pre-migration bytes so the original is preserved
		// even before the caller saves. Best-effort: a backup we cannot write must
		// never block startup — the on-disk config is untouched until the caller's
		// atomic Save, so proceeding without a fresh .bak is safe. (A prior .bak with
		// a restrictive ACL under the least-privilege service account was exactly what
		// crash-looped an upgrade.)
		if err := writeFileRestricted(path+".bak", raw); err != nil {
			slog.Warn("could not refresh config .bak before migration; proceeding without it",
				"path", path+".bak", "err", err)
		}
		if err := migrate(&cfg); err != nil {
			return nil, fmt.Errorf("migrate config: %w", err)
		}
	}

	if cfg.SessionSecret, err = unwrapStored(cfg.SessionSecret); err != nil {
		return nil, fmt.Errorf("session_secret: %w", err)
	}
	for i := range cfg.Remotes {
		r := &cfg.Remotes[i]
		if r.Address, err = unwrapStored(r.Address); err != nil {
			return nil, fmt.Errorf("remotes[%s].address: %w", r.ID, err)
		}
		if r.ClientKey, err = unwrapStored(r.ClientKey); err != nil {
			return nil, fmt.Errorf("remotes[%s].client_key: %w", r.ID, err)
		}
	}

	// Normalizing templates at load, not only on save, is what lets an install
	// that predates templates.mosaic pass validation: the GUID-binding applier
	// validates the loaded config without saving it, so a blank mosaic template
	// would fail there on every such install. It also makes the backslash rewrite
	// reach configs that are never re-saved.
	NormalizeTemplates(&cfg)
	NormalizeAllowedHosts(&cfg)

	return &cfg, nil
}

// wrapForStorage is a secret's stored form: base64(wrapSecret(plain)), empty
// for an empty secret.
func wrapForStorage(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	wrapped, err := wrapSecret([]byte(plain))
	if err != nil {
		return "", fmt.Errorf("wrap: %w", err)
	}
	return base64.StdEncoding.EncodeToString(wrapped), nil
}

// unwrapStored reverses wrapForStorage. The error never quotes the value.
func unwrapStored(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	wrapped, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", errors.New("decode: not base64")
	}
	plain, err := unwrapSecret(wrapped)
	if err != nil {
		return "", fmt.Errorf("unwrap: %w", err)
	}
	return string(plain), nil
}

// schemaVersionFor is the schema version cfg is saved at: a config holding a
// remote is marked so a binary that predates remotes refuses it rather than
// dropping its pairing keys on its next save; any other config keeps
// model.CurrentSchemaVersion, so such a binary still reads it.
func schemaVersionFor(cfg *model.Config) int {
	if len(cfg.Remotes) > 0 {
		return model.RemotesSchemaVersion
	}
	return model.CurrentSchemaVersion
}

// NormalizeAllowedHosts canonicalizes web.allowed_hosts in place, mirroring the
// way NormalizeTemplates canonicalizes folder templates. It never errors: the
// field has never been validated and is rendered on no surface, so hand-edited
// values are the population in the wild, and turning them into errors would 400
// every later config PUT and fail the GUID-binding applier on a field the
// operator never touched. Newly-submitted entries are the only ones that can be
// rejected, which is ValidateSubmittedHosts' job.
//
// An empty result becomes ["localhost"] so a hand-written config with the key
// deleted still admits the console; Defaults() already ships that value, so the
// rule only ever fires on hand-written YAML.
func NormalizeAllowedHosts(cfg *model.Config) {
	seen := make(map[string]bool, len(cfg.Web.AllowedHosts))
	out := make([]string, 0, len(cfg.Web.AllowedHosts))
	for _, h := range cfg.Web.AllowedHosts {
		h = NormalizeHostEntry(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		out = []string{"localhost"}
	}
	cfg.Web.AllowedHosts = out
}

// NormalizeHostEntry canonicalizes one allowlist entry: trim, lower-case, strip
// the root-label trailing dot, strip a ":port" suffix, and unwrap an IPv6
// literal's brackets.
//
// The port rule deliberately does not reuse the web layer's hostOnly, which is
// written for real Host headers and mangles anything else: hostOnly of
// "http://nas.local" is "http", so reusing it would silently rewrite a stored
// entry to "http" and then persist that on the operator's next unrelated save.
// Here the port is stripped only when what remains is plausibly a host, which
// leaves scheme-bearing entries intact for ValidateSubmittedHosts to reject.
func NormalizeHostEntry(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	if host, port, err := net.SplitHostPort(h); err == nil && host != "" && isPortDigits(port) {
		// A bare IPv6 literal splits too ("::1" → "::"/"1"), so only take the split
		// when the input was bracketed or nothing colon-ish is left in the host.
		if strings.HasPrefix(h, "[") || !strings.Contains(host, ":") {
			h = strings.TrimSuffix(host, ".")
		}
	}
	if len(h) > 1 && strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1] // "[::1]" → "::1", the form checkHost compares
	}
	return h
}

// isPortDigits reports whether s is a non-empty run of digits, i.e. a port.
func isPortDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// WriteFileRestricted atomically writes data to path with restricted
// permissions. It is exported for callers outside this package that must persist
// a secret-bearing file — the web layer's one-time setup token — so the
// "restrict the file BEFORE writing any secret into it" ordering is shared
// rather than re-derived.
//
// Exposing this rather than the bare restrictFile is deliberate: a bare restrict
// helper can only be used as create → write → restrict, which leaves the
// credential readable for the duration of the write. On Windows that is a real
// exposure, because mode bits are ignored there and the installer's DATADIR
// lockdown covers only the default data directory — a -data-dir elsewhere is
// created with a mode Windows does not honour.
func WriteFileRestricted(path string, data []byte) error {
	return writeFileRestricted(path, data)
}

// Save writes cfg to path atomically: it backs up the prior file to path+".bak"
// (with the restricted ACL), writes a temp file in the same directory whose ACL
// is restricted BEFORE any secret is written, fsyncs the file and its directory,
// then renames over the target. It stamps cfg.SchemaVersion (picked from the
// content, see schemaVersionFor) and cfg.UpdatedAt in place so the caller's value
// reflects what was persisted (design architecture §7, §8).
func Save(path string, cfg *model.Config) error {
	cfg.SchemaVersion = schemaVersionFor(cfg)
	cfg.UpdatedAt = time.Now().UnixNano()

	// Serialize from a shallow copy so the caller keeps the plaintext secrets
	// while the file gets the wrapped form. Only the secret strings are replaced,
	// and the remotes slice is copied before its secrets are; every other shared
	// slice/map is read, never mutated.
	toWrite := *cfg
	var err error
	if toWrite.SessionSecret, err = wrapForStorage(cfg.SessionSecret); err != nil {
		return fmt.Errorf("session_secret: %w", err)
	}
	if cfg.Remotes != nil {
		toWrite.Remotes = make([]model.RemoteProfile, len(cfg.Remotes))
		for i, r := range cfg.Remotes {
			if r.Address, err = wrapForStorage(r.Address); err != nil {
				return fmt.Errorf("remotes[%s].address: %w", r.ID, err)
			}
			if r.ClientKey, err = wrapForStorage(r.ClientKey); err != nil {
				return fmt.Errorf("remotes[%s].client_key: %w", r.ID, err)
			}
			toWrite.Remotes[i] = r
		}
	}

	data, err := yaml.Marshal(&toWrite)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	dir := filepath.Dir(path)

	// Back up the prior file first, with the same restricted ACL. The .bak roll on
	// a normal save is deliberate here as well; the web layer's optimistic-
	// concurrency check gates whether a save happens at all (design architecture
	// §8).
	if prior, err := os.ReadFile(path); err == nil {
		if err := writeFileRestricted(path+".bak", prior); err != nil {
			return fmt.Errorf("write .bak: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read prior config for .bak: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Error("failed to remove temp config", "path", tmpName, "err", err)
			}
		}
	}()

	// Restrict the temp file BEFORE writing any secret into it, so the session
	// secret never sits in a world-readable temp during the write window (design
	// architecture §7).
	if err := restrictFile(tmpName); err != nil {
		return fmt.Errorf("restrict temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp config over %s: %w", path, err)
	}
	renamed = true

	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("fsync config dir: %w", err)
	}
	slog.Info("config saved", "path", path, "schema_version", cfg.SchemaVersion, "updated_at", cfg.UpdatedAt)
	return nil
}

// writeFileRestricted atomically writes data to path with restricted permissions.
// It writes a fresh temp file in the same directory — ACL-restricted BEFORE the
// content, so a secret never lands in a world-readable file — fsyncs it, then
// renames over the target.
//
// The rename (rather than an in-place O_TRUNC of the existing file) is load-bearing
// for the .bak refresh: replacing a file needs write permission on the *directory*,
// not on the possibly-restrictive existing file. A prior .bak written under a
// different principal — e.g. an earlier run as LocalSystem — leaves an ACL the
// least-privilege Windows service account cannot open for writing, which is what
// crash-looped a config-migration upgrade. The atomic replace sidesteps it,
// the same way Save already writes config.yaml itself.
func writeFileRestricted(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cfgwrite-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Error("failed to remove temp file", "path", tmpName, "err", err)
			}
		}
	}()

	// Restrict the temp file BEFORE writing any secret-bearing content into it.
	if err := restrictFile(tmpName); err != nil {
		return fmt.Errorf("restrict %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp over %s: %w", path, err)
	}
	renamed = true
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}
