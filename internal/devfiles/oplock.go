package devfiles

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/rig"
)

// opLock is the one operation lock across the tool: at most one copy, upload,
// rename, delete, download or plan walk runs at a time. It never waits: a second operation is
// refused with ErrBusy. The holder's record lets the refusal say what runs, on
// which rigs, since when, and whether it has been cancelled but is still
// waiting on a silent device.
type opLock struct {
	mu     sync.Mutex
	holder *opHolder
}

type opHolder struct {
	op      string
	rigs    []string
	started time.Time
	ctx     context.Context
}

// lockOp takes the operation lock for op on rigIDs, held by the request whose
// context is ctx, or returns ErrBusy. op is the operation's display name
// ("Copy", "Delete", "Download", "Copy check", …); messages start with it. A
// request that needs two rigs takes this lock before either rig's slot: every
// two-slot request holds it, so two of them never wait on each other. unlock
// is called exactly once.
func (s *Service) lockOp(ctx context.Context, op string, rigIDs ...string) (unlock func(), err error) {
	s.op.mu.Lock()
	h := s.op.holder
	if h == nil {
		mine := &opHolder{op: op, rigs: rigIDs, started: s.now(), ctx: ctx}
		s.op.holder = mine
		s.op.mu.Unlock()
		return func() {
			s.op.mu.Lock()
			if s.op.holder == mine {
				s.op.holder = nil
			}
			s.op.mu.Unlock()
		}, nil
	}
	held := *h
	s.op.mu.Unlock()

	stopping := held.ctx.Err() != nil
	s.log.Warn("devfiles: operation refused: busy", "op", op, "rig", strings.Join(rigIDs, ","),
		"holder_op", held.op, "holder_rigs", strings.Join(held.rigs, ","),
		"holder_started", held.started, "holder_stopping", stopping)
	return nil, &Error{Kind: ErrBusy, Msg: s.busyMessage(held, stopping)}
}

// busyMessage words a refusal from the holder's record.
func (s *Service) busyMessage(h opHolder, stopping bool) string {
	names := make([]string, len(h.rigs))
	var wait time.Duration
	for i, id := range h.rigs {
		names[i] = s.nameOf(id)
		if t := s.rigSMBTimeout(id); t > wait {
			wait = t
		}
	}
	rigs := strings.Join(names, " and ")
	if stopping {
		return h.op + " on " + rigs + " is still stopping: waiting up to " +
			wait.Round(time.Second).String() + " for " + rigs + " to respond."
	}
	return h.op + " on " + rigs + " is running (started " + h.started.Format(time.RFC3339) +
		"). Try again when it has finished."
}

// rigSMBTimeout is the stall budget of the site rigID was last found on: how
// long a blocked call on it can take to return.
func (s *Service) rigSMBTimeout(rigID string) time.Duration {
	ep, _ := s.endpoint(rigID)
	return rig.SMBTimeout(s.sites.Site(ep.Site))
}
