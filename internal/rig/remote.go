package rig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// stallPasses is how many consecutive passes a remote rig may fail a pull or a
// delete, with its remote's tunnel up, before the stalled-progress alert: at
// the shipped 45 s poll about two minutes, long enough that one dropped frame
// is not an alert and short enough that a link flapping faster than a frame's
// transfer is reported while the night is still young.
const stallPasses = 3

// reasonRemoteDown is the Safe verdict's reason while the tunnel of the rig's
// remote is not up.
const reasonRemoteDown = model.PowerOffRemote

// setSite publishes the id of this cycle's site, and the display name the
// site reports when it is a supervised remote: the local site, and an unknown
// one (a remote never paired or since removed), names none.
func (w *Worker) setSite(site netsite.Site) {
	id := site.ID()
	if string(id) != w.st.Site {
		w.log.Info("rig: path switched", "from_site", w.st.Site, "to_site", string(id))
	}
	w.st.Site = string(id)
	w.st.RemoteName = ""
	if c := site.Costs(); c.Remote {
		w.st.RemoteName = c.RemoteName
	}
}

// remoteTunnelDown reports whether site, a cycle's, is a supervised remote
// whose tunnel is not up. An unknown site, one never paired or since removed,
// is not a remote that is down: its dial fails and the rig takes the ordinary
// offline path.
func remoteTunnelDown(site netsite.Site) bool {
	return site.Costs().Remote && site.State() != netsite.StateUp
}

// handleRemoteDown records a cycle skipped because the rig's remote's tunnel is
// not up. The rig's own offline state and alert apply only while that tunnel is
// up, so nothing is dialled, no rig.offline event or unreachable alert is
// raised, and the rig's unreachable timer is cleared so that it starts afresh
// once the tunnel is back. The hub shows the remote's state beside it
// (hub.remoteView).
func (w *Worker) handleRemoteDown(ctx context.Context, cfg *model.Config, profile *model.RigProfile) {
	if !w.unreachableSince.IsZero() || w.unreachableAlerted {
		w.log.Debug("rig: remote's tunnel is down; the rig's own unreachable timer restarts when it is back")
	}
	w.unreachableSince = time.Time{}
	w.unreachableAlerted = false
	w.goOffline(cfg, profile, reasonRemoteDown)
	w.log.Debug("rig: remote's tunnel is not up; skipping cycle", "site", w.st.Site)
	w.publish()
}

// setInSession publishes whether the worker holds a live path to the device.
func (w *Worker) setInSession(on bool) {
	if w.st.InSession == on {
		return
	}
	w.st.InSession = on
	w.publish()
}

// recordLastKnown keeps the verdict of a listing, for a card whose remote goes
// down to show what was last known.
func (w *Worker) recordLastKnown(safe model.SafeToPowerOff, reason string, at int64, pending int) {
	w.st.LastKnown = &model.LastKnownState{Safe: safe, Reason: reason, At: at, Pending: pending}
}

// linkFailures counts the failed operations of one phase of a cycle, and the
// frames they blocked, for the stalled-progress alert.
type linkFailures struct {
	mu      sync.Mutex
	n       int
	frames  map[string]bool // blocked by a failed stat or delete
	hashing int             // blocked by a failed pre-delete measurement
}

// note counts err when it is a link failure, blocking the frame at path ("" for
// an operation that is not one frame's stat or delete). Only network failures
// count: an absent file or the end of a stream is an answer, and an operation
// refused by the device (a read-only marker, a log it will not serve) says
// nothing about the remote's link.
func (f *linkFailures) note(err error, path string) {
	if err == nil || errors.Is(err, io.EOF) || !device.IsNetwork(err) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if path != "" {
		if f.frames == nil {
			f.frames = map[string]bool{}
		}
		f.frames[path] = true
	}
}

// fail counts a failure no session operation reported.
func (f *linkFailures) fail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
}

// addFrames counts n frames whose pre-delete measurement failed. A measurement
// is the only read a delete attempt makes and it never follows a failed stat,
// so these frames are not among those note recorded.
func (f *linkFailures) addFrames(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n += n
	f.hashing += n
}

func (f *linkFailures) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// blocked is the number of frames whose delete attempt failed.
func (f *linkFailures) blocked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames) + f.hashing
}

// watchDevice wraps dev so every failed open, mount, listing, stat, read and
// delete it carries is counted in f. It only observes: every call and result
// passes through unchanged, so no decision of the cycle depends on it.
func watchDevice(dev device.Device, f *linkFailures) device.Device {
	return watchedDevice{Device: dev, f: f}
}

type watchedDevice struct {
	device.Device
	f *linkFailures
}

func (d watchedDevice) Open(ctx context.Context) (device.Session, error) {
	s, err := d.Device.Open(ctx)
	d.f.note(err, "")
	if err != nil {
		return s, err
	}
	return watchedSession{Session: s, f: d.f}, nil
}

type watchedSession struct {
	device.Session
	f *linkFailures
}

func (s watchedSession) ListShares() ([]string, error) {
	out, err := s.Session.ListShares()
	s.f.note(err, "")
	return out, err
}

func (s watchedSession) Mount(share string) (device.Share, error) {
	sh, err := s.Session.Mount(share)
	s.f.note(err, "")
	if err != nil {
		return sh, err
	}
	return watchedShare{Share: sh, f: s.f}, nil
}

// watchedShare counts the operations a pull or a delete attempt is made of.
// Free space, volume info and markers are left out: they fail routinely on
// some media, and no frame waits on them.
type watchedShare struct {
	device.Share
	f *linkFailures
}

func (s watchedShare) ListDir(dir string) ([]device.FileInfo, error) {
	out, err := s.Share.ListDir(dir)
	s.f.note(err, "")
	return out, err
}

func (s watchedShare) Stat(path string) (device.FileInfo, error) {
	fi, err := s.Share.Stat(path)
	s.f.note(err, path)
	return fi, err
}

func (s watchedShare) OpenReader(path string) (io.ReadCloser, error) {
	rc, err := s.Share.OpenReader(path)
	// A read's failure reaches the delete attempt as its failed measurement
	// (addFrames), so it counts as a failure here and not as a blocked frame.
	s.f.note(err, "")
	if err != nil {
		return rc, err
	}
	return watchedReader{ReadCloser: rc, f: s.f}, nil
}

func (s watchedShare) Delete(path string) error {
	err := s.Share.Delete(path)
	s.f.note(err, path)
	return err
}

type watchedReader struct {
	io.ReadCloser
	f *linkFailures
}

func (r watchedReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.f.note(err, "")
	return n, err
}

// trackStall keeps the stalled-progress count after an online cycle and raises
// or retires its alert. progressed reports a frame copied or a device original
// deleted; pullFailed and deletes what failed on the way, a delete attempt
// being blocked once per frame however many of its operations failed.
//
// Only a remote rig counts passes, and only a pass whose tunnel was still up at
// its end: a pass cut by the tunnel going down is explained by the remote's
// own state, and a remote that is down raises its own alert. A pass that
// neither progressed nor failed, everything waiting for an ordinary reason
// (the settle window, a supersede in flight, an unclassified frame, a source
// deferral), leaves the count where it is, so such waits never feed it and a
// link failing between them still adds up. Any success ends the episode.
func (w *Worker) trackStall(ctx context.Context, cy cycle, progressed, pullFailed bool, deletes *linkFailures) {
	if progressed {
		if w.stallPasses > 0 {
			w.log.Info("rig: progress through the remote resumed", "failed_passes", w.stallPasses)
		}
		w.stallPasses, w.stallBlocked = 0, 0
		if w.stallRaised {
			w.resolveAlerts(ctx, model.AlertRemoteStalled)
			w.stallRaised = false
		}
		return
	}
	if ctx.Err() != nil || !cy.site.Costs().Remote || (!pullFailed && deletes.count() == 0) {
		return
	}
	if cy.site.State() != netsite.StateUp {
		w.log.Debug("rig: failed pass not counted; the remote's tunnel went down during it")
		return
	}
	w.stallPasses++
	w.stallBlocked += deletes.blocked()
	w.log.Debug("rig: pass through the remote made no progress", "failed_passes", w.stallPasses,
		"pull_failed", pullFailed, "delete_failures", deletes.count(), "deletes_blocked", deletes.blocked())
	// Raised once, as the count reaches the threshold: stallRaised starts true
	// to retire an alert a previous process left, so it cannot gate the raise.
	if w.stallPasses != stallPasses {
		return
	}
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: model.SeverityWarn,
		Category: model.AlertRemoteStalled,
		Message: fmt.Sprintf("rig %q is making no progress through remote %q: %d passes in a row failed while its tunnel was up, %d delete attempts blocked",
			cy.profile.Name, w.st.RemoteName, w.stallPasses, w.stallBlocked),
		Standing: true,
	})
	w.stallRaised = true
}
