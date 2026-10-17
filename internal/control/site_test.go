package control

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestDialerRoutesThroughSite: the production Dialer opens port 4700 through the
// site the endpoint names, and an unknown site fails with a net.Error before
// any socket is opened.
func TestDialerRoutesThroughSite(t *testing.T) {
	sites := netsite.NewRegistry(nil)
	site := netsite.NewFake("r1")
	if err := sites.Add(site); err != nil {
		t.Fatalf("add: %v", err)
	}
	dial := NewDialer(sites)

	// The fake site has no network, so the dial fails, but only after the site
	// was asked to carry it.
	if _, err := dial(context.Background(), netsite.Endpoint{Site: "r1", IP: "192.168.7.20"}, time.Second); err == nil {
		t.Fatal("dial through a fake site with no dialer succeeded")
	}
	if got, want := site.Calls(), []string{"dial 192.168.7.20:4700"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("site calls = %v, want %v", got, want)
	}

	_, err := dial(context.Background(), netsite.Endpoint{Site: "gone", IP: "192.168.7.20"}, time.Second)
	var ne net.Error
	if !errors.As(err, &ne) {
		t.Fatalf("dial through an unknown site = %v, want a net.Error", err)
	}
	if got := site.Calls(); len(got) != 1 {
		t.Fatalf("site calls after unknown-site dial = %v, want unchanged", got)
	}
}
