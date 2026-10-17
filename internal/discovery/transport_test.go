package discovery

import (
	"context"
	"reflect"
	"testing"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestUDPTransportProbeRoutesBySite: the unicast identity probe is carried by
// the site its endpoint names and by no other, and an endpoint naming an
// unknown site is never probed at all.
func TestUDPTransportProbeRoutesBySite(t *testing.T) {
	sites := netsite.NewRegistry(testLogger())
	site := netsite.NewFake("r1")
	site.SetProbeReply([]byte(`{"method":"scan_air","result":{"guid":"g1","cpuId":"c1"},"code":0}`), "192.168.7.20")
	if err := sites.Add(site); err != nil {
		t.Fatalf("add site: %v", err)
	}
	tr := newUDPTransport(sites, testLogger())

	dev, ok := tr.Probe(context.Background(), netsite.Endpoint{Site: "r1", IP: "192.168.7.20"}, scanRequest)
	if !ok || dev.GUID != "g1" || dev.CPUID != "c1" {
		t.Fatalf("Probe via r1 = %+v ok=%v, want g1/c1", dev, ok)
	}
	if got, want := site.Calls(), []string{"probe 192.168.7.20:4720"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("site calls = %v, want %v", got, want)
	}

	if _, ok := tr.Probe(context.Background(), netsite.Endpoint{Site: "gone", IP: "192.168.7.20"}, scanRequest); ok {
		t.Fatal("Probe via an unknown site answered, want refused")
	}
	if got := site.Calls(); len(got) != 1 {
		t.Fatalf("site calls after unknown-site probe = %v, want unchanged", got)
	}
}
