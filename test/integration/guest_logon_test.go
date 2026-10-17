//go:build integration

package integration

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	smb1 "github.com/macourteau/smb1client"
)

// TestServerGrantsGuestLogon pins the property the fixture exists to reproduce:
// the ASIAIR has no account matching the guest/12345678 login it documents, so
// it grants a guest session instead of authenticating that name. A dialer left
// on the default AllowGuest=false refuses such a session, which is what made
// every rig cycle fail and the device read as offline.
//
// It dials with AllowGuest=false deliberately. Should the Samba image ever
// regain a real account under that name, this fails and says so — otherwise the
// guard in TestGuestLogonSessionOpens would silently stop testing anything.
func TestServerGrantsGuestLogon(t *testing.T) {
	newDevice(t) // gates on reachability, skipping when no container is up

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host(), "445"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	sd := &smb1.Dialer{Initiator: &smb1.NTLMInitiator{User: "guest", Password: "12345678"}}
	sess, err := sd.DialContext(ctx, conn)
	if err == nil {
		sess.Logoff()
		t.Fatal("AllowGuest=false dial succeeded: the server authenticated the login as a real user, so the fixture no longer reproduces the ASIAIR's guest downgrade")
	}
	if !errors.Is(err, smb1.ErrGuestLogon) {
		t.Fatalf("AllowGuest=false dial failed for the wrong reason: %v (want %v)", err, smb1.ErrGuestLogon)
	}
}

// TestGuestLogonSessionOpens: the device path accepts the guest session the rig
// grants, and the session is usable.
func TestGuestLogonSessionOpens(t *testing.T) {
	dev := newDevice(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := dev.Open(ctx)
	if err != nil {
		t.Fatalf("Open against a guest-granting server: %v", err)
	}
	defer sess.Close()

	shares, err := sess.ListShares()
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	for _, s := range shares {
		if s == shareName {
			return
		}
	}
	t.Fatalf("ListShares = %v, want it to contain %q", shares, shareName)
}
