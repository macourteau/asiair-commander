package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
	"gopkg.in/yaml.v3"
)

const (
	testRemoteAddress = "tc-remote-address-carrying-a-psk"
	testRemoteKey     = "privkey:remote-client-key-secret"
)

func withRemote(cfg *model.Config) *model.Config {
	cfg.Remotes = []model.RemoteProfile{{
		ID: "r1", Name: "Observatory", Address: testRemoteAddress, ClientKey: testRemoteKey,
		Region: "fra", RegionName: "Frankfurt", RateLimitMBps: 2.5, CreatedAt: 1_700_000_000,
	}}
	cfg.RemoteSeq = 1
	return cfg
}

// storedSchema reads the schema_version a saved file carries, as any binary
// would before deciding whether to load it.
func storedSchema(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		SchemaVersion int `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(raw, &head); err != nil {
		t.Fatal(err)
	}
	return head.SchemaVersion
}

// TestConfigSchemaFollowsRemotes is design remote-sites AC25: a config with no
// remote saves at CurrentSchemaVersion, byte-for-byte free of any remote key,
// and round-trips; one with a remote saves at the bumped version, which the
// previous binary (whose newest version is CurrentSchemaVersion) refuses; and
// Load reads both without migrating either or refreshing the .bak.
func TestConfigSchemaFollowsRemotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	plain := Defaults()
	plain.Rigs = []model.RigProfile{} // "rigs: []" loads as empty, not nil
	if err := Save(path, plain); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := storedSchema(t, path); got != model.CurrentSchemaVersion {
		t.Fatalf("no remote: schema_version %d, want %d", got, model.CurrentSchemaVersion)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "remote") {
		t.Fatalf("a config with no remote carries a remote key:\n%s", raw)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, plain) {
		t.Fatalf("no-remote round trip mismatch:\n got=%+v\nwant=%+v", got, plain)
	}
	if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load of CurrentSchemaVersion wrote a .bak (err %v)", err)
	}

	paired := withRemote(Defaults())
	paired.Rigs = []model.RigProfile{}
	if err := Save(path, paired); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if paired.SchemaVersion != model.RemotesSchemaVersion {
		t.Fatalf("Save stamped %d, want %d", paired.SchemaVersion, model.RemotesSchemaVersion)
	}
	stored := storedSchema(t, path)
	if stored != model.RemotesSchemaVersion {
		t.Fatalf("with a remote: schema_version %d, want %d", stored, model.RemotesSchemaVersion)
	}
	// The previous binary's newest version is CurrentSchemaVersion; Load's own
	// gate is the same comparison against its constant.
	if stored <= model.CurrentSchemaVersion {
		t.Fatalf("schema_version %d would be read by a binary that predates remotes", stored)
	}
	bakBefore, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("Save wrote no .bak: %v", err)
	}
	got, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, paired) {
		t.Fatalf("remote round trip mismatch:\n got=%+v\nwant=%+v", got, paired)
	}
	if got.SchemaVersion != model.RemotesSchemaVersion {
		t.Fatalf("Load migrated the remotes version to %d", got.SchemaVersion)
	}
	if bakAfter, _ := os.ReadFile(path + ".bak"); string(bakAfter) != string(bakBefore) {
		t.Fatal("Load of the remotes version refreshed the .bak")
	}

	// Removing the last remote saves at CurrentSchemaVersion again.
	got.Remotes = nil
	if err := Save(path, got); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if v := storedSchema(t, path); v != model.CurrentSchemaVersion {
		t.Fatalf("after removing the last remote: schema_version %d, want %d", v, model.CurrentSchemaVersion)
	}

	// A version past the remotes one is still refused.
	if err := os.WriteFile(path, []byte("schema_version: 6\nweb_port: 8686\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("schema 6: want ErrSchemaTooNew, got %v", err)
	}
}

// TestConfigRedactRemotes is design remote-sites AC10's config half: the
// pairing secrets are wrapped on disk like session_secret, redacted from the
// GET form, kept by the clone, and a PUT of the whole config keeps the stored
// remotes whatever the body carries.
func TestConfigRedactRemotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := withRemote(Defaults())
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{testRemoteAddress, testRemoteKey} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("stored file carries a pairing secret verbatim")
		}
	}
	if cfg.Remotes[0].Address != testRemoteAddress {
		t.Fatal("Save mutated the caller's plaintext secret")
	}

	red := Redact(cfg)
	if red.Remotes[0].Address != model.RedactedSentinel || red.Remotes[0].ClientKey != model.RedactedSentinel {
		t.Fatalf("Redact left a pairing secret: %+v", red.Remotes[0])
	}
	if cfg.Remotes[0].Address != testRemoteAddress {
		t.Fatal("Redact mutated the source config")
	}
	body, err := json.Marshal(red)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testRemoteAddress, testRemoteKey} {
		if strings.Contains(string(body), secret) {
			t.Errorf("redacted JSON carries a pairing secret")
		}
	}

	clone := Clone(cfg)
	clone.Remotes[0].Name = "changed"
	if cfg.Remotes[0].Name != "Observatory" {
		t.Fatal("Clone shares the remotes slice")
	}

	// A PUT echoing the redacted form keeps the stored secrets.
	merged := ApplyRedactedWrite(cfg, red)
	if !reflect.DeepEqual(merged.Remotes, cfg.Remotes) || merged.RemoteSeq != cfg.RemoteSeq {
		t.Fatalf("redacted echo changed the remotes: %+v", merged.Remotes)
	}
	// A PUT that edits, adds, drops or empties remotes changes nothing.
	edited := Redact(cfg)
	edited.Remotes[0].Name = "renamed"
	edited.Remotes[0].Address = "tc-attacker-address"
	edited.Remotes = append(edited.Remotes, model.RemoteProfile{ID: "r2", Name: "extra", Address: "a", ClientKey: "k"})
	edited.RemoteSeq = 0
	for name, in := range map[string]*model.Config{
		"edited":  edited,
		"omitted": func() *model.Config { c := Redact(cfg); c.Remotes, c.RemoteSeq = nil, 0; return c }(),
	} {
		merged := ApplyRedactedWrite(cfg, in)
		if !reflect.DeepEqual(merged.Remotes, cfg.Remotes) || merged.RemoteSeq != cfg.RemoteSeq {
			t.Errorf("%s PUT changed the stored remotes: %+v seq %d", name, merged.Remotes, merged.RemoteSeq)
		}
	}
	// With nothing stored, a PUT cannot introduce a remote either.
	if merged := ApplyRedactedWrite(Defaults(), edited); merged.Remotes != nil || merged.RemoteSeq != 0 {
		t.Fatalf("a PUT introduced remotes: %+v", merged.Remotes)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Remotes[0].Address != testRemoteAddress || got.Remotes[0].ClientKey != testRemoteKey {
		t.Fatal("Load did not recover the pairing secrets")
	}
}

// TestRemoteSecretsStayInRestrictedFiles is design remote-sites §12 invariant
// 9's file half: a config holding a remote is saved, and its .bak rolled, with
// the owner-only mode a config with no remote gets, and both files carry the
// pairing secrets only in their wrapped form. Windows restricts these files with
// an ACL rather than mode bits, so there only the wrapping is checked.
func TestRemoteSecretsStayInRestrictedFiles(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain", "config.yaml")
	pairedPath := filepath.Join(dir, "paired", "config.yaml")
	for _, p := range []string{plainPath, pairedPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Two saves each, so the .bak is rolled from a file that holds the remote.
	for range 2 {
		if err := Save(plainPath, Defaults()); err != nil {
			t.Fatalf("Save without a remote: %v", err)
		}
		if err := Save(pairedPath, withRemote(Defaults())); err != nil {
			t.Fatalf("Save with a remote: %v", err)
		}
	}

	for _, name := range []string{"config.yaml", "config.yaml.bak"} {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(pairedPath), name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var stored struct {
			Remotes []model.RemoteProfile `yaml:"remotes"`
		}
		if err := yaml.Unmarshal(raw, &stored); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(stored.Remotes) != 1 {
			t.Fatalf("%s holds %d remotes, want the paired one", name, len(stored.Remotes))
		}
		for field, pair := range map[string][2]string{
			"address":    {stored.Remotes[0].Address, testRemoteAddress},
			"client_key": {stored.Remotes[0].ClientKey, testRemoteKey},
		} {
			got, want := pair[0], pair[1]
			if strings.Contains(string(raw), want) {
				t.Errorf("%s carries the %s verbatim", name, field)
			}
			if plain, err := unwrapStored(got); err != nil || plain != want {
				t.Errorf("%s: the stored %s does not unwrap to the secret (err %v)", name, field, err)
			}
		}

		if runtime.GOOS == "windows" {
			continue
		}
		plainInfo, err := os.Stat(filepath.Join(filepath.Dir(plainPath), name))
		if err != nil {
			t.Fatal(err)
		}
		pairedInfo, err := os.Stat(filepath.Join(filepath.Dir(pairedPath), name))
		if err != nil {
			t.Fatal(err)
		}
		if got := pairedInfo.Mode().Perm(); got != 0o600 || got != plainInfo.Mode().Perm() {
			t.Errorf("%s with a remote has mode %v, want %v, as without one (%v)",
				name, got, os.FileMode(0o600), plainInfo.Mode().Perm())
		}
	}
}

func TestValidateRemotes(t *testing.T) {
	cfg := withRemote(Defaults())
	if problems := validateRemotes(cfg); len(problems) != 0 {
		t.Fatalf("valid remote: %v", problems)
	}
	bad := withRemote(Defaults())
	bad.Remotes = append(bad.Remotes,
		model.RemoteProfile{ID: "r1", Name: "dup", Address: "a", ClientKey: "k"},
		model.RemoteProfile{ID: "r..2", Name: " ", RateLimitMBps: -1},
	)
	problems := validateRemotes(bad)
	want := map[string]bool{
		"remotes[r1].id": false, "remotes[r..2].id": false, "remotes[r..2].name": false,
		"remotes[r..2]": false, "remotes[r..2].rate_limit_mbps": false,
	}
	for _, p := range problems {
		if _, ok := want[p.Field]; ok {
			want[p.Field] = true
		}
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("no problem reported on %s (got %v)", f, problems)
		}
	}
}

// TestSchemaVersionTracksStayOrdered guards the two schema tracks against a
// future bump of model.CurrentSchemaVersion that forgets model.RemotesSchemaVersion:
// a remote-holding config must always stamp a version above
// CurrentSchemaVersion, so the previous binary refuses it and migrate never
// reads it as an intermediate version of the linear registry.
func TestSchemaVersionTracksStayOrdered(t *testing.T) {
	current, remotes, maxVersion := model.CurrentSchemaVersion, model.RemotesSchemaVersion, model.MaxSchemaVersion
	if remotes <= current {
		t.Fatalf("RemotesSchemaVersion %d is not above CurrentSchemaVersion %d; move it above and migrate files stamped with the old value",
			remotes, current)
	}
	if maxVersion != remotes {
		t.Fatalf("MaxSchemaVersion %d, want RemotesSchemaVersion %d", maxVersion, remotes)
	}
	if _, ok := migrations[remotes]; ok {
		t.Fatalf("migrations registers a step from RemotesSchemaVersion %d, which is not on the linear track", remotes)
	}
}
