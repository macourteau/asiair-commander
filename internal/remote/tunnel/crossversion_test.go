package tunnel

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runVar opts in to the test. It builds two binaries and needs the network and
// the release tags, so it runs in the tailcat gate (dependabot-automerge.yml)
// and on request, not in every build-test run.
const runVar = "REMOTE_CROSSVERSION"

// previousRefVar overrides the ref the test treats as the previous release. The
// default is the newest v* tag reachable from HEAD. Pointing it at HEAD runs the
// current build against itself, which proves the harness without a prior
// release.
const previousRefVar = "REMOTE_CROSSVERSION_PREVIOUS"

// remoteTestHooks is the file a build must carry to take part: the hidden
// remotetest-client verb. Its presence in a ref's tree is how the test tells a
// release that predates the remote role from one it can drive.
const remoteTestHooks = "cmd/asiair-commander/remotetest_verb.go"

// xvResult mirrors the fields of the remotetest-client verb's stdout that the
// test reads. The verb's interface is a contract between builds: it only grows.
type xvResult struct {
	Hello struct {
		BuildVersion       string `json:"build_version"`
		WholeDigestVersion int    `json:"whole_digest_version"`
		DataDigestVersion  int    `json:"data_digest_version"`
		InstanceID         string `json:"instance_id"`
		PathConnected      bool   `json:"path_connected"`
	} `json:"hello"`
	BroadcastReplies int    `json:"broadcast_replies"`
	BroadcastCode    string `json:"broadcast_code"`
	HashCode         string `json:"hash_code"`
}

// TestRemoteCrossVersion runs the remote role of one build against the
// Commander-side client of another, in both directions, through a real tunnel
// and a local relay (never public DERP). It is how a tailcat or tailscale.com
// bump that changes the wire format fails: the link never comes up, so no
// hello arrives.
//
// Both binaries are built with the remotetest tag, which admits the local test
// relay and adds the hidden remotetest-client verb; the previous release is
// built from its own source, so the test needs no private image and no secret.
//
// It runs only when REMOTE_CROSSVERSION is set, and then a skip must mean only
// the one known state: it skips, with the reason, until a release exists whose
// source carries the remote role and the remotetest hooks, since before that
// there is no previous build to run against. The first such release therefore
// has nothing to compare with, and the second is the first to be compared.
// Anything else that leaves no previous build (no release tag found, a ref
// that does not resolve, a build that fails) fails the test: a skip passes the
// tailcat gate, so a broken checkout must not read as "nothing to compare".
func TestRemoteCrossVersion(t *testing.T) {
	if os.Getenv(runVar) == "" {
		t.Skipf("set %s=1 to run: it builds two binaries and runs only in the tailcat gate", runVar)
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ref := os.Getenv(previousRefVar)
	if ref == "" {
		out, err := exec.Command("git", "-C", root, "describe", "--tags", "--abbrev=0", "--match", "v*").Output()
		if err != nil {
			t.Fatalf("no previous release tag to compare with (git describe: %v); the checkout needs its tags (fetch-depth: 0), or set %s to override", err, previousRefVar)
		}
		ref = strings.TrimSpace(string(out))
	}
	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run(); err != nil {
		t.Fatalf("previous release ref %q does not resolve to a commit: %v", ref, err)
	}
	if err := exec.Command("git", "-C", root, "cat-file", "-e", ref+":"+remoteTestHooks).Run(); err != nil {
		t.Skipf("previous release %s has no remote role with the remotetest hooks (%s); the cross-version test starts with the release after the first one that has them", ref, remoteTestHooks)
	}

	dir := t.TempDir()
	newBin := buildRemoteTestBinary(t, root, "", filepath.Join(dir, "new"))
	oldSrc := filepath.Join(dir, "old-src")
	if err := os.Mkdir(oldSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := exec.Command("git", "-C", root, "archive", "--format=tar", ref)
	untar := exec.Command("tar", "-x", "-C", oldSrc)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	untar.Stdin = pipe
	if err := untar.Start(); err != nil {
		t.Fatalf("tar: %v", err)
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("git archive %s: %v", ref, err)
	}
	if err := untar.Wait(); err != nil {
		t.Fatalf("tar: %v", err)
	}
	oldBin := buildRemoteTestBinary(t, oldSrc, ref, filepath.Join(dir, "old"))

	t.Logf("previous release: %s", ref)
	for _, d := range []struct{ name, remote, client string }{
		{"new remote, previous Commander", newBin, oldBin},
		{"previous remote, new Commander", oldBin, newBin},
	} {
		t.Run(d.name, func(t *testing.T) { runCrossVersion(t, d.remote, d.client) })
	}
}

// buildRemoteTestBinary builds the commander binary in srcDir with the
// remotetest tag.
func buildRemoteTestBinary(t *testing.T, srcDir, ref, out string) string {
	t.Helper()
	cmd := exec.Command("go", "build", "-tags", "remotetest", "-o", out, "./cmd/asiair-commander")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build (%s): %v\n%s", cmp.Or(ref, "current source"), err, b)
	}
	return out
}

// runCrossVersion runs remoteBin's remote role behind a fresh local relay and
// drives it from clientBin's remotetest-client verb.
func runCrossVersion(t *testing.T, remoteBin, clientBin string) {
	relay := startLocalRelay(t)
	p, err := Mint(relay.region)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var remoteLog bytes.Buffer
	remote := exec.Command(remoteBin, "remote")
	remote.Env = append(os.Environ(), "ASIAIR_REMOTE_PAIRING="+p.EnvValue.Reveal(), "TS_DEBUG_ADDR=")
	remote.Stdout, remote.Stderr = &remoteLog, &remoteLog
	if err := remote.Start(); err != nil {
		t.Fatalf("start remote: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- remote.Wait() }()
	t.Cleanup(func() {
		remote.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			remote.Process.Kill()
			<-done
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := exec.CommandContext(ctx, clientBin, "remotetest-client")
	client.Env = append(os.Environ(),
		"ASIAIR_REMOTETEST_ADDRESS="+p.Address.Reveal(),
		"ASIAIR_REMOTETEST_CLIENTKEY="+p.ClientKey.Reveal())
	var stdout, stderr bytes.Buffer
	client.Stdout, client.Stderr = &stdout, &stderr
	if err := client.Run(); err != nil {
		select {
		case rerr := <-done:
			t.Fatalf("remotetest-client: %v\nstderr: %s\nthe remote exited (%v):\n%s", err, stderr.String(), rerr, remoteLog.String())
		default:
		}
		t.Fatalf("remotetest-client: %v\nstderr: %s\nremote log:\n%s", err, stderr.String(), remoteLog.String())
	}

	var res xvResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("remotetest-client output: %v\n%s", err, stdout.String())
	}
	t.Logf("remote %s answered hello", res.Hello.BuildVersion)
	var problems []error
	check := func(ok bool, msg string) {
		if !ok {
			problems = append(problems, errors.New(msg))
		}
	}
	check(res.Hello.InstanceID != "", "hello carried no instance id")
	check(res.Hello.PathConnected, "hello did not report the tunnel path as connected")
	check(res.Hello.WholeDigestVersion > 0 && res.Hello.DataDigestVersion > 0, "hello carried no digest versions")
	// Broadcast on a CI runner's LAN answers either way; both are protocol-level
	// replies that prove the framing.
	check(res.BroadcastCode == "" || res.BroadcastCode == "network", "broadcast failed with code "+res.BroadcastCode)
	check(res.HashCode == "refused", "hash of an unlisted address answered "+res.HashCode+", want refused")
	if err := errors.Join(problems...); err != nil {
		t.Fatalf("%v\nresult: %+v\nremote log:\n%s", err, res, remoteLog.String())
	}
}
