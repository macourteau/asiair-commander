package device

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	smb1 "github.com/macourteau/smb1client"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestSMBDeviceDialsThroughSite: Probe and Open dial TCP/445 through the site
// the endpoint names, and an endpoint whose site is unknown fails both with a
// network error, so the worker takes its ordinary unreachable path.
func TestSMBDeviceDialsThroughSite(t *testing.T) {
	sites := netsite.NewRegistry(nil)
	site := netsite.NewFake("r1")
	if err := sites.Add(site); err != nil {
		t.Fatalf("add: %v", err)
	}
	site.OnDial(func(string, int) (net.Conn, error) {
		client, server := net.Pipe()
		server.Close()
		return client, nil
	})

	dev := New(sites, netsite.Endpoint{Site: "r1", IP: "192.168.7.20"}, "guest", "12345678", time.Second)
	if !dev.Probe(context.Background()) {
		t.Fatal("Probe through an up site that connects = false, want true")
	}
	if got, want := site.Calls(), []string{"dial 192.168.7.20:445"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("site calls = %v, want %v", got, want)
	}

	gone := New(sites, netsite.Endpoint{Site: "gone", IP: "192.168.7.20"}, "guest", "12345678", time.Second)
	if gone.Probe(context.Background()) {
		t.Fatal("Probe through an unknown site = true, want false")
	}
	_, err := gone.Open(context.Background())
	var ua *netsite.UnavailableError
	if !errors.As(err, &ua) || !smb1.IsNetworkError(err) {
		t.Fatalf("Open through an unknown site = %v, want a network error naming the site", err)
	}
	if got := site.Calls(); len(got) != 1 {
		t.Fatalf("site calls after unknown-site dials = %v, want unchanged", got)
	}
}
