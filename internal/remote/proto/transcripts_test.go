package proto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// transcript is one recorded call (testdata/transcripts/README.md).
type transcript struct {
	Version int               `json:"version"`
	Op      Op                `json:"op"`
	Request json.RawMessage   `json:"request"`
	Replies []json.RawMessage `json:"replies"`
}

func (tr transcript) final() json.RawMessage { return tr.Replies[len(tr.Replies)-1] }

// loadTranscripts reads every recorded version's transcripts, keyed
// "v<N>/<file>".
func loadTranscripts(t *testing.T) map[string]transcript {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("testdata", "transcripts", "v*"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]transcript{}
	seen := map[int]bool{}
	for _, dir := range dirs {
		v, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "v"))
		if err != nil {
			t.Fatalf("transcript directory %s is not v<N>", dir)
		}
		seen[v] = true
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		ops := map[Op]bool{}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var tr transcript
			if err := json.Unmarshal(b, &tr); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if tr.Version != v || len(tr.Replies) == 0 {
				t.Fatalf("%s: version %d with %d replies, want version %d and a reply", f, tr.Version, len(tr.Replies), v)
			}
			ops[tr.Op] = true
			out[filepath.Join(filepath.Base(dir), filepath.Base(f))] = tr
		}
		for _, op := range []Op{OpHello, OpBroadcast, OpHash} {
			if !ops[op] {
				t.Errorf("%s records no %s call", dir, op)
			}
		}
	}
	// Every version this build speaks has a recording, so a version added
	// without one fails here.
	for _, v := range Versions {
		if !seen[v] {
			t.Errorf("protocol version %d has no recorded transcript", v)
		}
	}
	return out
}

// TestRemoteProtocolTranscripts replays every recorded version in both roles:
// the current server answers each recorded request in a form the recorded
// client reads, and the current client accepts each recorded reply and writes
// a request the recorded server reads.
func TestRemoteProtocolTranscripts(t *testing.T) {
	for name, tr := range loadTranscripts(t) {
		t.Run(name+"/server", func(t *testing.T) { replayServer(t, tr) })
		t.Run(name+"/client", func(t *testing.T) { replayClient(t, tr) })
	}
}

// replayHandler answers with the recorded result or error, decoded with the
// current types, and reports the recorded progress before a hash's result.
type replayHandler struct {
	t     *testing.T
	final reply
	steps []int64
}

func (h replayHandler) Hello(context.Context, string) HelloReply { return *h.final.Hello }

func (h replayHandler) Broadcast(context.Context, string) (BroadcastReply, error) {
	if h.final.Error != nil {
		return BroadcastReply{}, h.final.Error
	}
	return *h.final.Broadcast, nil
}

func (h replayHandler) Hash(_ context.Context, _ string, _ HashRequest, progress func(int64)) (HashReply, error) {
	for _, n := range h.steps {
		progress(n)
		// Long enough for the progress ticker to send it.
		time.Sleep(30 * time.Millisecond)
	}
	if h.final.Error != nil {
		return HashReply{}, h.final.Error
	}
	return *h.final.Hash, nil
}

func replayServer(t *testing.T, tr transcript) {
	h := replayHandler{t: t}
	if err := json.Unmarshal(tr.final(), &h.final); err != nil {
		t.Fatalf("current types cannot decode the recorded reply: %v", err)
	}
	for _, raw := range tr.Replies[:len(tr.Replies)-1] {
		var p reply
		if err := json.Unmarshal(raw, &p); err != nil || p.Progress == nil {
			t.Fatalf("recorded non-final reply is not progress: %s", raw)
		}
		h.steps = append(h.steps, p.Progress.Bytes)
	}

	client, server := net.Pipe()
	defer client.Close()
	go Serve(server, h, ServeOptions{ProgressInterval: 5 * time.Millisecond})
	client.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Write(append(compact(t, tr.Request), '\n')); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	var progress []json.RawMessage
	for {
		line, err := readFrame(br, maxReplyFrame)
		if err != nil {
			t.Fatalf("reading the current server's reply: %v", err)
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		json.Unmarshal(line, &kind)
		if kind.Kind == kindProgress {
			progress = append(progress, line)
			continue
		}
		if err := contains(decode(t, line), decode(t, tr.final()), true, "reply"); err != nil {
			t.Errorf("the current server's reply drops what a recorded client reads: %v\ncurrent:  %s\nrecorded: %s", err, line, tr.final())
		}
		break
	}
	if len(h.steps) > 0 && len(progress) == 0 {
		t.Errorf("the current server sent no progress where the recorded one did")
	}
	for _, rec := range tr.Replies[:len(tr.Replies)-1] {
		for _, cur := range progress {
			if err := contains(decode(t, cur), decode(t, rec), false, "progress"); err != nil {
				t.Errorf("the current server's progress frame lacks a recorded field: %v", err)
			}
		}
	}
}

func replayClient(t *testing.T, tr transcript) {
	var req request
	if err := json.Unmarshal(tr.Request, &req); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	dial := func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			server.SetDeadline(time.Now().Add(10 * time.Second))
			line, err := readFrame(bufio.NewReader(server), maxRequestFrame)
			if err != nil {
				served <- err
				return
			}
			// The recorded server reads the recorded request's fields.
			if err := contains(decode(t, line), decode(t, tr.Request), true, "request"); err != nil {
				served <- fmt.Errorf("the current client's request drops what a recorded server reads: %v\ncurrent:  %s\nrecorded: %s", err, line, tr.Request)
				return
			}
			for _, r := range tr.Replies {
				if _, err := server.Write(append(compact(t, r), '\n')); err != nil {
					served <- err
					return
				}
			}
			served <- nil
		}()
		return client, nil
	}
	c := NewClient(dial, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var final reply
	if err := json.Unmarshal(tr.final(), &final); err != nil {
		t.Fatal(err)
	}
	var got any
	var err error
	switch tr.Op {
	case OpHello:
		var hr HelloReply
		hr, _, err = c.Hello(ctx, req.CallID)
		got = hr
	case OpBroadcast:
		var dgs []Datagram
		dgs, err = c.Broadcast(ctx, req.CallID)
		got = BroadcastReply{Replies: dgs}
	case OpHash:
		var seen []int64
		var hr HashReply
		hr, err = c.Hash(ctx, req.CallID, *req.Hash, HashOptions{Stall: 5 * time.Second, OnProgress: func(n int64) { seen = append(seen, n) }})
		got = hr
		if want := len(tr.Replies) - 1; err == nil && len(seen) != want {
			t.Errorf("client reported %d progress frames, want %d", len(seen), want)
		}
	default:
		t.Fatalf("unknown op %q", tr.Op)
	}
	if serr := <-served; serr != nil {
		t.Fatal(serr)
	}

	if final.Kind == kindError {
		if CodeOf(err) != final.Error.Code {
			t.Fatalf("client returned %v, want the recorded %q error", err, final.Error.Code)
		}
		return
	}
	if err != nil {
		t.Fatalf("client rejected the recorded reply: %v", err)
	}
	b, _ := json.Marshal(got)
	var payload map[string]json.RawMessage
	json.Unmarshal(tr.final(), &payload)
	if err := contains(decode(t, b), decode(t, payload[string(tr.Op)]), true, string(tr.Op)); err != nil {
		t.Errorf("client lost a recorded field: %v\ndecoded:  %s\nrecorded: %s", err, b, payload[string(tr.Op)])
	}
}

func compact(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// decode reads JSON with numbers kept exact, so a nanosecond mtime compares
// digit for digit.
func decode(t *testing.T, raw []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// contains reports whether cur carries everything rec does: every key of a
// recorded object, recursively, and every element of a recorded array (by
// membership for scalars, a later version may list more versions; by position
// for objects). values=false compares shape only.
func contains(cur, rec any, values bool, path string) error {
	switch r := rec.(type) {
	case map[string]any:
		c, ok := cur.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: is %T, recorded an object", path, cur)
		}
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cv, ok := c[k]
			if !ok {
				return fmt.Errorf("%s.%s: missing", path, k)
			}
			if err := contains(cv, r[k], values, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	case []any:
		c, ok := cur.([]any)
		if !ok {
			return fmt.Errorf("%s: is %T, recorded an array", path, cur)
		}
		for i, re := range r {
			if _, obj := re.(map[string]any); obj {
				if i >= len(c) {
					return fmt.Errorf("%s[%d]: missing", path, i)
				}
				if err := contains(c[i], re, values, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
				continue
			}
			found := false
			for _, ce := range c {
				if contains(ce, re, values, path) == nil {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%s: recorded element %v missing", path, re)
			}
		}
		return nil
	default:
		if fmt.Sprintf("%T", cur) != fmt.Sprintf("%T", rec) {
			return fmt.Errorf("%s: is %T, recorded %T", path, cur, rec)
		}
		if values && cur != rec {
			return fmt.Errorf("%s: is %v, recorded %v", path, cur, rec)
		}
		return nil
	}
}
