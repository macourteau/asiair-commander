package device

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	smb1 "github.com/macourteau/smb1client"
)

// recorderConn records every deadline armed against it, standing in for
// net.Conn.SetDeadline without a real socket.
type recorderConn struct {
	deadlines []time.Time
}

func (c *recorderConn) setDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return nil
}

type fakeCloser struct{ closed bool }

func (c *fakeCloser) Close() error {
	c.closed = true
	return nil
}

// A deadline is armed before every Read so a stalled transfer errors out while
// a progressing one keeps extending its window.
func TestDeadlineReaderRearmsBeforeEachRead(t *testing.T) {
	rec := &recorderConn{}
	dr := &deadlineReader{
		r:           bytes.NewReader([]byte("hello world")),
		setDeadline: rec.setDeadline,
		timeout:     time.Second,
	}
	buf := make([]byte, 4)
	reads := 0
	for {
		n, err := dr.Read(buf)
		if n > 0 {
			reads++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}
	}
	if reads == 0 {
		t.Fatal("expected at least one data read")
	}
	if len(rec.deadlines) < reads {
		t.Fatalf("expected a deadline armed per read: reads=%d armed=%d", reads, len(rec.deadlines))
	}
	for i, d := range rec.deadlines {
		if d.IsZero() {
			t.Fatalf("deadline %d was the zero time, expected a future deadline", i)
		}
	}
}

// Close clears the shared deadline (zero time) and closes the underlying file.
func TestDeadlineReaderCloseClearsDeadline(t *testing.T) {
	rec := &recorderConn{}
	c := &fakeCloser{}
	dr := &deadlineReader{
		r:           bytes.NewReader(nil),
		closer:      c,
		setDeadline: rec.setDeadline,
		timeout:     time.Second,
	}
	if err := dr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(rec.deadlines) == 0 {
		t.Fatal("Close did not touch the deadline")
	}
	last := rec.deadlines[len(rec.deadlines)-1]
	if !last.IsZero() {
		t.Fatalf("Close should clear the deadline to the zero time, got %v", last)
	}
	if !c.closed {
		t.Fatal("Close did not close the underlying file")
	}
}

// A stalled read honours the deadline via a real net.Conn instead of blocking.
func TestDeadlineReaderTimesOutOnStall(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	dr := &deadlineReader{
		r:           c1,
		closer:      c1,
		setDeadline: c1.SetDeadline,
		timeout:     50 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		_, err := dr.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected a timeout error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read blocked instead of honouring the idle deadline")
	}
}

// A deadline is armed before every Write so a stalled upload errors out while
// a progressing one keeps extending its window.
func TestDeadlineWriterRearmsBeforeEachWrite(t *testing.T) {
	rec := &recorderConn{}
	var sink bytes.Buffer
	dw := &deadlineWriter{
		w:           &sink,
		setDeadline: rec.setDeadline,
		timeout:     time.Second,
	}
	chunks := []string{"hell", "o wo", "rld"}
	for _, c := range chunks {
		if _, err := dw.Write([]byte(c)); err != nil {
			t.Fatalf("unexpected write error: %v", err)
		}
	}
	if sink.String() != "hello world" {
		t.Fatalf("wrote %q", sink.String())
	}
	if len(rec.deadlines) < len(chunks) {
		t.Fatalf("expected a deadline armed per write: writes=%d armed=%d", len(chunks), len(rec.deadlines))
	}
	for i, d := range rec.deadlines {
		if d.IsZero() {
			t.Fatalf("deadline %d was the zero time, expected a future deadline", i)
		}
	}
}

// Close arms the idle deadline for the SMB CLOSE, then clears the shared
// deadline (zero time), and closes the underlying file.
func TestDeadlineWriterCloseClearsDeadline(t *testing.T) {
	rec := &recorderConn{}
	c := &fakeCloser{}
	dw := &deadlineWriter{
		w:           io.Discard,
		closer:      c,
		setDeadline: rec.setDeadline,
		timeout:     time.Second,
	}
	if err := dw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(rec.deadlines) < 2 || rec.deadlines[0].IsZero() {
		t.Fatalf("Close should arm a deadline and then clear it, got %v", rec.deadlines)
	}
	last := rec.deadlines[len(rec.deadlines)-1]
	if !last.IsZero() {
		t.Fatalf("Close should clear the deadline to the zero time, got %v", last)
	}
	if !c.closed {
		t.Fatal("Close did not close the underlying file")
	}
}

// A stalled write honours the deadline via a real net.Conn instead of blocking.
func TestDeadlineWriterTimesOutOnStall(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	dw := &deadlineWriter{
		w:           c1,
		closer:      c1,
		setDeadline: c1.SetDeadline,
		timeout:     50 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		// Nobody reads c2, so the write stalls.
		_, err := dw.Write([]byte("data"))
		done <- err
	}()
	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected a timeout error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked instead of honouring the idle deadline")
	}
}

// stallCloser stands in for an SMB file whose CLOSE request is written to a
// device that never reads it.
type stallCloser struct{ conn net.Conn }

func (c stallCloser) Close() error {
	_, err := c.conn.Write([]byte("close"))
	return err
}

// A Close the device never answers honours the idle deadline instead of
// blocking the copy, and the operation lock with it.
func TestDeadlineWriterCloseTimesOutOnStall(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	dw := &deadlineWriter{
		w:           c1,
		closer:      stallCloser{c1},
		setDeadline: c1.SetDeadline,
		timeout:     50 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() { done <- dw.Close() }()
	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected a timeout error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked instead of honouring the idle deadline")
	}
}

// deadlineConn records the deadlines armed on a real connection.
type deadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return c.Conn.SetDeadline(t)
}

// The session's teardown (Umount, Logoff) is bounded by a deadline, so a
// silent device cannot hold the session's Close forever.
func TestSMBSessionCloseArmsDeadline(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	conn := &deadlineConn{Conn: c1}
	s := &smbSession{sess: &smb1.Session{}, conn: conn, opTimeout: time.Second}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(conn.deadlines) == 0 || conn.deadlines[0].IsZero() {
		t.Fatalf("Close armed no deadline before the teardown: %v", conn.deadlines)
	}
}
