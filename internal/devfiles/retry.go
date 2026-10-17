package devfiles

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
)

// rigError attributes a failure to the rig whose side it came from, so a
// cross-rig operation's exhaustion names the right device.
type rigError struct {
	rig string
	err error
}

func (e *rigError) Error() string { return e.err.Error() }
func (e *rigError) Unwrap() error { return e.err }

// onRig tags err with the rig whose device produced it (nil stays nil). A
// cross-rig engine wraps its reader's and writer's errors with it.
func onRig(rigID string, err error) error {
	if err == nil {
		return nil
	}
	return &rigError{rig: rigID, err: err}
}

// retryable reports the two kinds of failure withRetry retries: a network
// failure (tunnel errors and netsite.UnavailableError included) and an
// unanswered identity probe. Everything else, a refused logon included, is
// returned at once.
func retryable(err error) bool {
	return device.IsNetwork(err) || errors.Is(err, errNoProbeReply)
}

// cancelled is the error for an operation whose request context ended.
func cancelled(ctx context.Context) error {
	return &Error{Kind: ErrCancelled, Msg: "cancelled", Err: ctx.Err()}
}

// withRetry runs fn on freshly checked-out sessions of rigIDs (passed to fn in
// the same order; a rig named twice gets one session), retrying exactly two
// kinds of failure (retryable) up to len(s.backoffs) more times after each
// backoff, every attempt on fresh sessions: the failed ones are closed at
// checkin. It checks ctx before classifying an error and waits out each
// backoff on ctx, so a cancel returns ErrCancelled, never a retry. Any other
// error returns at once. Exhaustion returns ErrUnreachable naming the rig
// whose side failed last.
//
// A two-rig caller must hold the operation lock (lockOp) before calling it.
// share is for the log lines only.
func (s *Service) withRetry(ctx context.Context, op, share string, rigIDs []string,
	fn func(ss []*session) error) error {
	var last error
	lastRig := rigIDs[0]
	attempts := 0
	for {
		if err := ctx.Err(); err != nil {
			return cancelled(ctx)
		}
		attempts++
		failedRig, err := s.attempt(ctx, op, rigIDs, fn)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return cancelled(ctx)
		}
		if !retryable(err) {
			return err
		}
		last, lastRig = err, failedRig
		if attempts > len(s.backoffs) {
			break
		}
		wait := s.backoffs[attempts-1]
		s.log.Warn("devfiles: retrying", "rig", lastRig, "share", share, "op", op,
			"attempt", attempts, "backoff", wait, "error", err)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return cancelled(ctx)
		}
	}
	name := s.nameOf(lastRig)
	s.log.Error("devfiles: retries exhausted", "rig", lastRig, "share", share, "op", op,
		"attempts", attempts, "error", last)
	return &Error{
		Kind: ErrUnreachable,
		Rig:  lastRig,
		Msg:  fmt.Sprintf("%s is unreachable (tried %d times). Check that it is on and connected.", name, attempts),
		Err:  last,
	}
}

// withRig is withRetry for one rig.
func (s *Service) withRig(ctx context.Context, op, share, rigID string, fn func(c *session) error) error {
	return s.withRetry(ctx, op, share, []string{rigID}, func(ss []*session) error { return fn(ss[0]) })
}

// attempt checks out every rig's session, runs fn and checks them back in
// with its error. It reports the rig a failure is attributed to.
func (s *Service) attempt(ctx context.Context, op string, rigIDs []string,
	fn func(ss []*session) error) (failedRig string, err error) {
	byRig := map[string]*session{}
	var order []*session
	releaseAll := func(err error) {
		for _, c := range order {
			s.release(c, err)
		}
	}
	ss := make([]*session, len(rigIDs))
	for i, id := range rigIDs {
		if c, ok := byRig[id]; ok {
			ss[i] = c
			continue
		}
		c, err := s.acquire(ctx, id, op)
		if err != nil {
			// The sessions already out are healthy: park them.
			releaseAll(nil)
			return id, err
		}
		byRig[id] = c
		order = append(order, c)
		ss[i] = c
	}
	// Deferred so a panic in fn still returns the rig slots. A panicking fn
	// leaves its sessions in an unknown state, so they are closed, not parked.
	returned := false
	defer func() {
		if !returned {
			s.log.Error("devfiles: operation panicked; closing its sessions", "rigs", rigIDs, "op", op)
			for _, c := range order {
				c.broken = true
			}
		}
		releaseAll(err)
	}()
	err = fn(ss)
	returned = true
	failedRig = rigIDs[0]
	var re *rigError
	if errors.As(err, &re) {
		failedRig = re.rig
	}
	return failedRig, err
}
