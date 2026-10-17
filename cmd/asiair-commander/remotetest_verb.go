//go:build remotetest

// The hidden `remotetest-client` verb, compiled only under `-tags remotetest`,
// is the Commander side of the cross-version test (TestRemoteCrossVersion in
// internal/remote/tunnel): it dials one remote through a real tunnel and
// exercises hello, broadcast and hash, so a build can be run against another
// build's `remote` role.
//
// Its interface is a contract between builds, because the test runs it from the
// previous release's source as well as the current one: the pairing arrives in
// ASIAIR_REMOTETEST_ADDRESS and ASIAIR_REMOTETEST_CLIENTKEY, and one JSON object
// goes to stdout. Add fields; never rename or remove one.
//
// It is dispatched from init, before main, so it touches neither the verb
// switch in main.go nor any production code path.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

const remoteTestVerb = "remotetest-client"

func init() {
	if len(os.Args) > 1 && os.Args[1] == remoteTestVerb {
		os.Exit(remoteTestClient())
	}
}

// remoteTestResult is the verb's stdout.
type remoteTestResult struct {
	// Hello is the remote's hello reply, reduced to what a build pair must
	// agree on.
	Hello struct {
		BuildVersion       string `json:"build_version"`
		WholeDigestVersion int    `json:"whole_digest_version"`
		DataDigestVersion  int    `json:"data_digest_version"`
		InstanceID         string `json:"instance_id"`
		PathConnected      bool   `json:"path_connected"`
	} `json:"hello"`
	// BroadcastReplies is the number of scan_air replies the remote returned;
	// BroadcastCode is the remote's error code when the round failed on its LAN
	// (a protocol-level answer, which also proves the framing), "" otherwise.
	BroadcastReplies int    `json:"broadcast_replies"`
	BroadcastCode    string `json:"broadcast_code"`
	// HashCode is the remote's error code for a hash of an address that never
	// answered a broadcast: "refused".
	HashCode string `json:"hash_code"`
}

func remoteTestClient() int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "remotetest-client: "+format+"\n", a...)
		return 1
	}
	address, clientKey := os.Getenv("ASIAIR_REMOTETEST_ADDRESS"), os.Getenv("ASIAIR_REMOTETEST_CLIENTKEY")
	tc, err := tunnel.NewClient(tunnel.ClientConfig{Address: tunnel.Secret(address), ClientKey: tunnel.Secret(clientKey)})
	if err != nil {
		return fail("new client: %v", err)
	}
	defer tc.Close()
	pc := proto.NewClient(tc.DialProtocol, nil)
	defer pc.Close()

	// The first hello brings the tunnel up through the relay.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var out remoteTestResult
	hr, _, err := pc.Hello(ctx, "xv-hello")
	if err != nil {
		return fail("hello: %v", err)
	}
	out.Hello.BuildVersion = hr.Build.Version
	out.Hello.WholeDigestVersion = hr.WholeDigestVersion
	out.Hello.DataDigestVersion = hr.DataDigestVersion
	out.Hello.InstanceID = hr.InstanceID
	out.Hello.PathConnected = hr.Path.Connected

	dgs, err := pc.Broadcast(ctx, "xv-broadcast")
	switch {
	case err == nil:
		out.BroadcastReplies = len(dgs)
	case proto.CodeOf(err) != "":
		out.BroadcastCode = string(proto.CodeOf(err))
	default:
		return fail("broadcast: %v", err)
	}

	// 192.0.2.1 is TEST-NET-1: it cannot have answered a broadcast, so the
	// remote must refuse it without opening any session.
	_, err = pc.Hash(ctx, "xv-hash", proto.HashRequest{IP: "192.0.2.1", Share: "x", Path: "x"}, proto.HashOptions{Stall: 10 * time.Second})
	if code := proto.CodeOf(err); code != "" {
		out.HashCode = string(code)
	} else {
		return fail("hash: expected a remote error reply, got %v", err)
	}

	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return fail("encode: %v", err)
	}
	return 0
}
