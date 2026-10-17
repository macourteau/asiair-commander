package hub

import (
	"net/netip"

	"github.com/macourteau/asiair-commander/internal/model"
)

// remoteView attaches to a rig's snapshot entry the display fields that come
// from the remote its last resolved endpoint names (nil on the local network,
// or for a site no paired remote has). It never changes what the worker, the
// rig's one writer, published: State, Online and the Safe verdict are the
// worker's, which writes the remote-down state itself when its remote's
// tunnel is down (the supervisor wakes it on every remote state change).
//
// RefusedForwards are the remote's refused forwards at the rig's address.
// While the remote's tunnel is not up (Reconnecting, Unreachable, Waiting),
// Remote carries its state and last hello, with Down set once the worker has
// written its remote-down state; LastKnown, the verdict of the rig's last
// listing, is shown only then.
func remoteView(r model.RigStatus, remote *model.RemoteStatus) model.RigStatus {
	r.Remote = nil
	r.RefusedForwards = nil
	last := r.LastKnown
	r.LastKnown = nil
	if r.Site == "" || remote == nil {
		return r
	}
	r.RefusedForwards = refusedFor(remote.Warnings, r.Address)
	switch remote.State {
	case model.RemoteReconnecting, model.RemoteUnreachable, model.RemoteWaiting:
	default:
		return r
	}
	down := r.PowerOffReason == model.PowerOffRemote && !r.InSession
	r.Remote = &model.RigRemote{State: remote.State, LastHelloAt: remote.LastHelloAt, Down: down}
	if down {
		r.LastKnown = last
	}
	return r
}

// refusedFor returns the refused-forward warnings whose target is addr.
func refusedFor(warnings []model.RemoteWarning, addr string) []model.RemoteWarning {
	if addr == "" {
		return nil
	}
	var out []model.RemoteWarning
	for _, w := range warnings {
		if w.Kind != model.RemoteWarningRefusedForward {
			continue
		}
		ap, err := netip.ParseAddrPort(w.Target)
		if err != nil || ap.Addr().String() != addr {
			continue
		}
		out = append(out, w)
	}
	return out
}
