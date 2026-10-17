package config

import (
	"errors"

	"github.com/macourteau/asiair-commander/internal/model"
)

// ErrStaleWrite is returned by CheckVersion when an incoming PUT carries an
// UpdatedAt that no longer matches the stored config — a concurrent edit landed
// first. The web layer maps this to HTTP 409 (design architecture §8).
var ErrStaleWrite = errors.New("stale config write: config changed since it was read")

// Redact returns a deep copy of cfg with every secret replaced by
// model.RedactedSentinel for GET /api/config. An empty secret is left empty so
// the first-run signal (no password set) survives redaction (design
// architecture §8).
func Redact(cfg *model.Config) *model.Config {
	out := cloneConfig(cfg)
	if out == nil {
		return nil
	}
	if out.PasswordHash != "" {
		out.PasswordHash = model.RedactedSentinel
	}
	if out.SessionSecret != "" {
		out.SessionSecret = model.RedactedSentinel
	}
	for i := range out.Rigs {
		if out.Rigs[i].Password != "" {
			out.Rigs[i].Password = model.RedactedSentinel
		}
	}
	for i := range out.Remotes {
		if out.Remotes[i].Address != "" {
			out.Remotes[i].Address = model.RedactedSentinel
		}
		if out.Remotes[i].ClientKey != "" {
			out.Remotes[i].ClientKey = model.RedactedSentinel
		}
	}
	return out
}

// ApplyRedactedWrite produces the config to persist from a PUT body. Any secret
// field equal to model.RedactedSentinel keeps the stored value; any other value
// (including empty, which clears the secret) replaces it. Rig passwords are
// matched to the stored rig by ID (design architecture §8).
//
// The remotes and their id sequence are not the PUT's to change: whatever the
// body carries, the stored value is kept, so a redacted remote can never
// overwrite its pairing secrets and only the remote supervisor adds, edits or
// removes one.
func ApplyRedactedWrite(stored, incoming *model.Config) *model.Config {
	out := cloneConfig(incoming)
	if out == nil {
		return nil
	}

	out.Remotes, out.RemoteSeq = nil, 0
	if stored != nil {
		out.Remotes = cloneRemotes(stored.Remotes)
		out.RemoteSeq = stored.RemoteSeq
	}

	if out.PasswordHash == model.RedactedSentinel {
		out.PasswordHash = storedString(stored, func(c *model.Config) string { return c.PasswordHash })
	}
	if out.SessionSecret == model.RedactedSentinel {
		out.SessionSecret = storedString(stored, func(c *model.Config) string { return c.SessionSecret })
	}

	for i := range out.Rigs {
		if out.Rigs[i].Password != model.RedactedSentinel {
			continue
		}
		// A sentinel with no matching stored rig (a new rig sending the placeholder)
		// has no prior secret to preserve, so it collapses to empty rather than
		// persisting the sentinel as a literal password.
		out.Rigs[i].Password = ""
		if stored != nil {
			if prior := stored.Rig(out.Rigs[i].ID); prior != nil {
				out.Rigs[i].Password = prior.Password
			}
		}
	}
	return out
}

// CheckVersion enforces optimistic concurrency: it returns ErrStaleWrite unless
// incoming.UpdatedAt matches stored.UpdatedAt (design architecture §8).
func CheckVersion(stored, incoming *model.Config) error {
	if stored == nil || incoming == nil {
		return nil
	}
	if incoming.UpdatedAt != stored.UpdatedAt {
		return ErrStaleWrite
	}
	return nil
}

func storedString(stored *model.Config, get func(*model.Config) string) string {
	if stored == nil {
		return ""
	}
	return get(stored)
}

// Clone deep-copies a config, so a writer can edit the copy of the live config
// and store it whole.
func Clone(c *model.Config) *model.Config { return cloneConfig(c) }

// cloneConfig deep-copies a config so redaction and write-merging never mutate
// the caller's value.
func cloneConfig(c *model.Config) *model.Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.ExcludeExtensions = cloneStrings(c.ExcludeExtensions)
	cp.DeleteSidecarExtensions = cloneStrings(c.DeleteSidecarExtensions)
	cp.AliasTable = cloneStrMap(c.AliasTable)
	cp.SensorMap = cloneStrMap(c.SensorMap)
	cp.Web.AllowedHosts = cloneStrings(c.Web.AllowedHosts)

	if c.Templates.Cal != nil {
		m := make(map[model.FrameType]string, len(c.Templates.Cal))
		for k, v := range c.Templates.Cal {
			m[k] = v
		}
		cp.Templates.Cal = m
	}

	if c.Rigs != nil {
		rigs := make([]model.RigProfile, len(c.Rigs))
		for i := range c.Rigs {
			rigs[i] = cloneRig(&c.Rigs[i])
		}
		cp.Rigs = rigs
	}
	cp.Remotes = cloneRemotes(c.Remotes)
	return &cp
}

// cloneRemotes copies the remote profiles; each holds only values.
func cloneRemotes(r []model.RemoteProfile) []model.RemoteProfile {
	if r == nil {
		return nil
	}
	out := make([]model.RemoteProfile, len(r))
	copy(out, r)
	return out
}

func cloneRig(r *model.RigProfile) model.RigProfile {
	cp := *r
	if r.Sources != nil {
		s := make([]model.Source, len(r.Sources))
		for i := range r.Sources {
			s[i] = model.Source{Share: r.Sources[i].Share, Roots: cloneStrings(r.Sources[i].Roots), Disabled: r.Sources[i].Disabled}
		}
		cp.Sources = s
	}
	cp.Grace = cloneDurPtr(r.Grace)
	cp.PollInterval = cloneDurPtr(r.PollInterval)
	cp.RetryInterval = cloneDurPtr(r.RetryInterval)
	cp.MinFileAge = cloneDurPtr(r.MinFileAge)
	cp.SettleCycles = cloneIntPtr(r.SettleCycles)
	return cp
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneDurPtr(p *model.Duration) *model.Duration {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
