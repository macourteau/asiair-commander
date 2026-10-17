package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Dialer opens one remote-protocol connection (tunnel.Client.DialProtocol).
type Dialer func(ctx context.Context) (net.Conn, error)

// Client is Commander's side of the remote protocol for one remote.
//
// Every call is idempotent, so a transport failure is retried with short
// backoff, but only within the call's own deadline: nothing is queued beyond
// it, because a result acts on facts that go stale. Hello rides one long-lived
// connection; broadcast and each hash get a connection of their own, so a hello
// never queues behind them and its round trip stays a clean latency sample.
type Client struct {
	dial Dialer
	log  *slog.Logger

	mu   sync.Mutex // serialises hellos on conn
	conn net.Conn
	br   *bufio.Reader

	vmu  sync.Mutex
	peer []int // the versions the remote's last hello reported
}

// NewClient builds a client that dials through dial. A nil logger uses
// slog.Default().
func NewClient(dial Dialer, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{dial: dial, log: log}
}

const (
	// HelloTimeout and BroadcastTimeout bound a call whose context carries no
	// deadline of its own, and are the bases the supervisor adds a site's
	// margins to. A broadcast round listens for about two seconds on the
	// remote's LAN.
	HelloTimeout     = 10 * time.Second
	BroadcastTimeout = 15 * time.Second

	// backoffMin and backoffMax bound the wait between attempts.
	backoffMin = 200 * time.Millisecond
	backoffMax = 2 * time.Second
)

// ErrStalled is a hash abandoned because no progress arrived within the
// caller's stall window.
var ErrStalled = errors.New("remote protocol: hash stalled: no progress within the stall window")

// errProtocol is a peer that broke the protocol; retrying would not help.
var errProtocol = errors.New("remote protocol: protocol violation")

// version is the protocol version to write in: the newest one both sides
// speak, or V1 before any hello, since every remote speaks V1.
func (c *Client) version() int {
	c.vmu.Lock()
	defer c.vmu.Unlock()
	best := V1
	for _, v := range c.peer {
		if v > best && speaks(v) {
			best = v
		}
	}
	return best
}

// Close drops the long-lived hello connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked()
	return nil
}

func (c *Client) dropLocked() {
	if c.conn != nil {
		c.conn.Close()
		c.conn, c.br = nil, nil
	}
}

// Hello asks the remote to describe itself. rtt is the round trip of the
// attempt that succeeded, the remote's latency sample.
func (c *Client) Hello(ctx context.Context, callID string) (hr HelloReply, rtt time.Duration, err error) {
	ctx, cancel := withDefaultDeadline(ctx, HelloTimeout)
	defer cancel()
	err = retry(ctx, func() (bool, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.conn == nil {
			conn, err := c.dial(ctx)
			if err != nil {
				return true, err
			}
			c.conn, c.br = conn, bufio.NewReader(conn)
		}
		start := time.Now()
		r, err := exchange(ctx, c.conn, c.br, request{V: c.version(), CallID: callID, Op: OpHello}, nil, nil)
		if err != nil {
			var re *Error
			if !errors.As(err, &re) {
				// The connection's state is unknown after a failed exchange.
				c.dropLocked()
			}
			return retryable(err), err
		}
		if r.Hello == nil {
			c.dropLocked()
			return false, fmt.Errorf("%w: hello result without a payload", errProtocol)
		}
		hr, rtt = *r.Hello, time.Since(start)
		return false, nil
	})
	if err != nil {
		return HelloReply{}, 0, err
	}
	c.vmu.Lock()
	c.peer = append([]int(nil), hr.Versions...)
	c.vmu.Unlock()
	return hr, rtt, nil
}

// Broadcast asks the remote to run one scan_air round on its LAN with its own
// request, and returns the raw replies.
func (c *Client) Broadcast(ctx context.Context, callID string) ([]Datagram, error) {
	ctx, cancel := withDefaultDeadline(ctx, BroadcastTimeout)
	defer cancel()
	var out []Datagram
	err := retry(ctx, func() (bool, error) {
		conn, err := c.dial(ctx)
		if err != nil {
			return true, err
		}
		defer conn.Close()
		r, err := exchange(ctx, conn, bufio.NewReader(conn), request{V: c.version(), CallID: callID, Op: OpBroadcast}, nil, nil)
		if err != nil {
			return retryable(err), err
		}
		if r.Broadcast == nil {
			return false, fmt.Errorf("%w: broadcast result without a payload", errProtocol)
		}
		out = r.Broadcast.Replies
		return false, nil
	})
	return out, err
}

// HashOptions are a hash call's stall rule and progress sink.
type HashOptions struct {
	// Stall is the stall window: the call gives up when no progress has
	// arrived for this long, and retries a dropped connection only within it.
	// It is the site's SMB budget, which the caller supplies. Required.
	Stall time.Duration
	// OnProgress, if set, receives each progress report: the bytes the
	// current attempt has read. Commander counts it as the tunnel moving bytes.
	OnProgress func(bytes int64)
}

// Hash asks the remote to measure one file in its own SMB session. It has no
// whole-call deadline: it runs while progress keeps arriving, and fails with
// ErrStalled when none arrives within opts.Stall. Cancelling ctx closes the
// connection, which stops the remote's read.
func (c *Client) Hash(ctx context.Context, callID string, req HashRequest, opts HashOptions) (HashReply, error) {
	if opts.Stall <= 0 {
		return HashReply{}, errors.New("remote protocol: hash needs a stall window")
	}
	last := time.Now()
	progress := func(n int64) {
		last = time.Now()
		if opts.OnProgress != nil {
			opts.OnProgress(n)
		}
	}
	backoff := backoffMin
	for {
		hr, err := c.hashOnce(ctx, callID, req, &last, opts.Stall, progress)
		if err == nil {
			return hr, nil
		}
		if ctx.Err() != nil {
			return HashReply{}, ctx.Err()
		}
		if !retryable(err) {
			return HashReply{}, err
		}
		left := opts.Stall - time.Since(last)
		if left <= backoff {
			return HashReply{}, fmt.Errorf("%w (last error: %v)", ErrStalled, err)
		}
		c.log.Debug("remote protocol: hash attempt failed; retrying", "call_id", callID, "error", err, "backoff", backoff)
		if !sleep(ctx, backoff) {
			return HashReply{}, ctx.Err()
		}
		backoff = min(2*backoff, backoffMax)
	}
}

// hashOnce is one attempt on a fresh connection. Every read is bounded by the
// stall window since the last progress.
func (c *Client) hashOnce(ctx context.Context, callID string, req HashRequest, last *time.Time, stall time.Duration, progress func(int64)) (HashReply, error) {
	dctx, cancel := context.WithDeadline(ctx, last.Add(stall))
	conn, err := c.dial(dctx)
	cancel()
	if err != nil {
		return HashReply{}, err
	}
	defer conn.Close()
	readDeadline := func() time.Time { return last.Add(stall) }
	r, err := exchange(ctx, conn, bufio.NewReader(conn), request{V: c.version(), CallID: callID, Op: OpHash, Hash: &req}, progress, readDeadline)
	if err != nil {
		return HashReply{}, err
	}
	if r.Hash == nil {
		return HashReply{}, fmt.Errorf("%w: hash result without a payload", errProtocol)
	}
	return *r.Hash, nil
}

// exchange writes req on conn and reads frames until its result or error.
// readDeadline, when set, bounds each read (the hash stall rule); otherwise
// ctx's deadline bounds the exchange. Cancelling ctx aborts it.
func exchange(ctx context.Context, conn net.Conn, br *bufio.Reader, req request, progress func(int64), readDeadline func() time.Time) (reply, error) {
	// A cancelled call unblocks its read by expiring the connection's
	// deadline; the caller drops or closes the connection afterwards.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	b, err := json.Marshal(req)
	if err != nil {
		return reply{}, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetWriteDeadline(dl)
	} else {
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return reply{}, ctxErr(ctx, err)
	}
	for {
		switch {
		case readDeadline != nil:
			conn.SetReadDeadline(readDeadline())
		default:
			dl, _ := ctx.Deadline()
			conn.SetReadDeadline(dl)
		}
		// Setting the deadline above could undo the cancel's expired one, so
		// a call cancelled before it is caught here instead.
		if err := ctx.Err(); err != nil {
			return reply{}, err
		}
		line, err := readFrame(br, maxReplyFrame)
		if err != nil {
			if errors.Is(err, errFrameTooLarge) {
				return reply{}, fmt.Errorf("%w: %v", errProtocol, err)
			}
			return reply{}, ctxErr(ctx, err)
		}
		var r reply
		if err := json.Unmarshal(line, &r); err != nil {
			return reply{}, fmt.Errorf("%w: malformed reply", errProtocol)
		}
		if r.CallID != req.CallID {
			// A remote that cannot read the request answers with no call id.
			if r.Kind == kindError && r.CallID == "" && r.Error != nil {
				return reply{}, r.Error
			}
			return reply{}, fmt.Errorf("%w: reply for call %q, want %q", errProtocol, r.CallID, req.CallID)
		}
		switch r.Kind {
		case kindProgress:
			if progress != nil && r.Progress != nil {
				progress(r.Progress.Bytes)
			}
		case kindError:
			if r.Error == nil {
				return reply{}, fmt.Errorf("%w: error reply without an error", errProtocol)
			}
			return reply{}, r.Error
		case kindResult:
			return r, nil
		default:
			// A later version's frame kind, which this build ignores.
		}
	}
}

// ctxErr prefers the context's error when the context ended, since a
// cancelled call shows up on the connection as an expired deadline.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// retryable reports whether a failed attempt may be retried: a transport
// failure may, while a remote's error reply, a protocol violation or an ended
// context may not.
func retryable(err error) bool {
	var re *Error
	switch {
	case errors.As(err, &re), errors.Is(err, errProtocol),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

// retry runs attempt until it succeeds, reports its error as final, or ctx
// ends, backing off between attempts.
func retry(ctx context.Context, attempt func() (again bool, err error)) error {
	backoff := backoffMin
	for {
		again, err := attempt()
		if err == nil || !again {
			return err
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= backoff {
			return err
		}
		if !sleep(ctx, backoff) {
			return err
		}
		backoff = min(2*backoff, backoffMax)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// withDefaultDeadline gives ctx a deadline when it has none, so a call's
// retries always end.
func withDefaultDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
