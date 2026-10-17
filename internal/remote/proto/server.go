package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Handler is the remote role's side of the three calls. Each runs with a
// context cancelled when the client goes away, and each call id is the one
// Commander generated, for the remote's log lines and warnings.
type Handler interface {
	// Hello describes the remote. Serve fills in Versions.
	Hello(ctx context.Context, callID string) HelloReply
	// Broadcast runs one scan_air round with the remote's own request.
	Broadcast(ctx context.Context, callID string) (BroadcastReply, error)
	// Hash measures one file, calling progress with the bytes read so far. It
	// must stop reading once ctx ends.
	Hash(ctx context.Context, callID string, req HashRequest, progress func(bytes int64)) (HashReply, error)
}

// ServeOptions tunes Serve; the zero value is the production setting.
type ServeOptions struct {
	// Logger receives the per-call log lines; nil uses slog.Default().
	Logger *slog.Logger
	// ProgressInterval is how often a hash's progress is sent while its byte
	// count moves; zero uses progressInterval.
	ProgressInterval time.Duration
}

const (
	// progressInterval is how often a running hash reports its progress. The
	// client's stall window is tens of seconds, so a few seconds keeps a
	// reading hash comfortably alive without chattering.
	progressInterval = 2 * time.Second
	// writeTimeout bounds one frame's write, so a client that stops reading
	// cannot hold a handler forever.
	writeTimeout = 30 * time.Second
	// maxQueued bounds requests read ahead of the one being served. A client
	// waits for each reply, so more than this is a misbehaving peer.
	maxQueued = 8
)

// Serve answers calls on conn until the client closes it or misbehaves, then
// closes conn. Calls on one connection run one at a time; concurrent calls use
// concurrent connections. Closing the connection cancels the call in flight.
func Serve(conn net.Conn, h Handler, opts ServeOptions) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	interval := opts.ProgressInterval
	if interval <= 0 {
		interval = progressInterval
	}
	s := &serverConn{conn: conn, h: h, log: log, interval: interval}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The reader keeps reading while a call runs, so a client that closes the
	// connection cancels the call at once rather than when it next writes.
	reqs := make(chan []byte, maxQueued)
	go func() {
		defer cancel()
		br := bufio.NewReader(conn)
		for {
			line, err := readFrame(br, maxRequestFrame)
			if err != nil {
				if errors.Is(err, errFrameTooLarge) {
					log.Warn("remote protocol: dropping connection: request frame too large")
				}
				return
			}
			select {
			case reqs <- line:
			default:
				log.Warn("remote protocol: dropping connection: too many queued requests")
				return
			}
		}
	}()

	for {
		select {
		case line := <-reqs:
			if !s.handle(ctx, line) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

type serverConn struct {
	conn     net.Conn
	h        Handler
	log      *slog.Logger
	interval time.Duration
	wmu      sync.Mutex
}

// send writes one frame; a write failure ends the connection.
func (s *serverConn) send(r reply) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = s.conn.Write(b)
	return err
}

func (s *serverConn) fail(v int, callID string, code ErrorCode, msg string) bool {
	return s.send(reply{V: v, CallID: callID, Kind: kindError, Error: &Error{Code: code, Message: msg}}) == nil
}

// handle serves one request and reports whether the connection is still
// usable.
func (s *serverConn) handle(ctx context.Context, line []byte) bool {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.log.Warn("remote protocol: malformed request")
		return s.fail(V1, "", CodeBadRequest, "malformed request")
	}
	// Replies are written in the request's version, so the client reads them
	// with the schema it asked in.
	v := req.V
	if !validCallID(req.CallID) {
		s.log.Warn("remote protocol: request without a valid call id", "op", string(req.Op))
		return s.fail(V1, "", CodeBadRequest, "missing or malformed call id")
	}
	log := s.log.With("call_id", req.CallID, "op", string(req.Op))
	if !speaks(v) {
		log.Warn("remote protocol: unsupported version", "version", v)
		return s.fail(V1, req.CallID, CodeUnsupported, "unsupported protocol version")
	}

	start := time.Now()
	var out reply
	var err error
	switch req.Op {
	case OpHello:
		hr := s.h.Hello(ctx, req.CallID)
		hr.Versions = append([]int(nil), Versions...)
		out = reply{Hello: &hr}
	case OpBroadcast:
		var br BroadcastReply
		br, err = s.h.Broadcast(ctx, req.CallID)
		out = reply{Broadcast: &br}
	case OpHash:
		if req.Hash == nil {
			log.Warn("remote protocol: hash request without arguments")
			return s.fail(v, req.CallID, CodeBadRequest, "hash request without arguments")
		}
		var hr HashReply
		hr, err = s.hash(ctx, v, req.CallID, *req.Hash)
		out = reply{Hash: &hr}
	default:
		log.Warn("remote protocol: unsupported op")
		return s.fail(v, req.CallID, CodeUnsupported, "unsupported op")
	}

	if err != nil {
		e := asError(err)
		// The handler wrote the message for the wire; it carries no call
		// argument, so it is safe to log too.
		log.Debug("remote protocol: call failed", "code", string(e.Code), "error", e.Message, "elapsed", time.Since(start))
		return s.fail(v, req.CallID, e.Code, e.Message)
	}
	log.Debug("remote protocol: call answered", "elapsed", time.Since(start))
	out.V, out.CallID, out.Kind = v, req.CallID, kindResult
	if err := s.send(out); err != nil {
		log.Debug("remote protocol: reply not delivered", "error", err)
		return false
	}
	return true
}

// hash runs the handler's hash while a ticker sends its progress whenever the
// byte count has moved since the last frame. A stalled read sends nothing, so
// the client's stall rule sees the stall.
func (s *serverConn) hash(ctx context.Context, v int, callID string, req HashRequest) (HashReply, error) {
	var read, sent atomic.Int64
	sent.Store(-1)
	done := make(chan struct{})
	ticker := time.NewTicker(s.interval)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				n := read.Load()
				if n == sent.Load() {
					continue
				}
				sent.Store(n)
				if err := s.send(reply{V: v, CallID: callID, Kind: kindProgress, Progress: &Progress{Bytes: n}}); err != nil {
					return
				}
			}
		}
	}()
	hr, err := s.h.Hash(ctx, callID, req, func(n int64) { read.Store(n) })
	close(done)
	wg.Wait()
	return hr, err
}

// asError turns a handler's error into the wire error. A handler that did not
// choose a code gets CodeFailed.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeFailed, Message: err.Error()}
}
