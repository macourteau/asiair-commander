package role

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
)

// Hash implements proto.Handler: it measures one file's digests in the
// remote's own SMB session, after a unicast scan_air probe inside that session,
// with the streaming read and digest code Commander's local pre-delete hash
// uses (a fresh OpenReader through header.StreamDigests, no cache). It never
// receives an expected value and never compares: Commander decides.
//
// The credentials live only for this call. Neither they nor any other argument
// reaches a log line or an error's text; the call is logged by its call id.
func (r *Remote) Hash(ctx context.Context, callID string, req proto.HashRequest, progress func(int64)) (proto.HashReply, error) {
	log := r.log.With("call_id", callID)
	start := time.Now()
	fail := func(stage string, err error) error {
		code := proto.CodeFailed
		switch {
		case ctx.Err() != nil:
			err = errors.New("call cancelled")
		case device.IsNotFound(err):
			code = proto.CodeNotFound
		case device.IsNetwork(err):
			code = proto.CodeNetwork
		}
		msg := stage + ": " + redactArgs(err.Error(), req)
		log.Info("remote: hash failed", "stage", stage, "code", string(code), "error", msg)
		return &proto.Error{Code: code, Message: msg}
	}

	ip, err := netip.ParseAddr(req.IP)
	if err != nil || !r.allow.has(ip) {
		log.Warn("remote: hash refused: address is not on the forwarding allowlist")
		return proto.HashReply{}, &proto.Error{Code: proto.CodeRefused, Message: "address is not on the remote's forwarding allowlist"}
	}
	ep := netsite.LocalEndpoint(ip.Unmap().String())
	log.Debug("remote: hash started")

	sess, err := r.newDevice(ep, req.User.Reveal(), req.Password.Reveal(), smbTimeout).Open(ctx)
	if err != nil {
		return proto.HashReply{}, fail("open session", err)
	}
	defer sess.Close()

	// The probe goes out only once the session is open, so the identity it
	// returns is the device this session talks to.
	var out proto.HashReply
	guid, cpuID, ok := r.scanner.ProbeGUID(ctx, ep)
	if ok {
		out.GUID, out.CPUID = guid, cpuID
	} else {
		log.Info("remote: identity probe unanswered inside the hash session")
	}

	share, err := sess.Mount(req.Share)
	if err != nil {
		return proto.HashReply{}, fail("mount", err)
	}
	fi, err := share.Stat(req.Path)
	if err != nil {
		return proto.HashReply{}, fail("stat", err)
	}
	out.Size, out.MtimeNS = fi.Size, fi.Mtime.UnixNano()

	rc, err := share.OpenReader(req.Path)
	if err != nil {
		return proto.HashReply{}, fail("open", err)
	}
	defer rc.Close()
	// A cancelled call stops at the next buffer; every read reports the bytes
	// read so far.
	var read int64
	stop := func() bool { return ctx.Err() != nil }
	report := func(n int64) { read = n; progress(n) }
	if out.Whole, out.Data, err = header.StreamDigests(rc, stop, report); err != nil {
		return proto.HashReply{}, fail("read", err)
	}
	log.Info("remote: hash measured", "bytes", read, "identified", out.Identified(), "elapsed", time.Since(start))
	return out, nil
}

// redactArgs removes every argument of a hash call from an error's text, so a
// failure can be logged and reported without naming the device, the file or
// the credentials. Longer values go first, so one that contains another (a
// path holding the share's name) is removed whole.
func redactArgs(s string, req proto.HashRequest) string {
	// SMB reports paths with either separator, so both forms of the path go.
	args := []string{req.IP, req.Share, req.Path, strings.ReplaceAll(req.Path, "/", `\`),
		strings.ReplaceAll(req.Path, `\`, "/"), req.User.Reveal(), req.Password.Reveal()}
	sort.Slice(args, func(i, j int) bool { return len(args[i]) > len(args[j]) })
	var pairs []string
	for _, a := range args {
		if a != "" {
			pairs = append(pairs, a, "[redacted]")
		}
	}
	return strings.NewReplacer(pairs...).Replace(s)
}
