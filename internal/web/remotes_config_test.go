package web

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
)

// TestConfigRedactRemotesOverAPI is the API half of TestConfigRedactRemotes
// (design remote-sites AC10): GET /api/config never returns a pairing secret,
// and PUT /api/config keeps the stored remotes whether its body echoes them,
// edits them or omits them, while still applying its other edits.
func TestConfigRedactRemotesOverAPI(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	const address, clientKey = "tc-secret-address-with-psk", "privkey:secret-client-key"
	stored := config.Clone(h.cfgPtr.Load())
	stored.Remotes = []model.RemoteProfile{{ID: "r3", Name: "Observatory", Address: address, ClientKey: clientKey, Region: "fra", CreatedAt: 1}}
	stored.RemoteSeq = 3
	h.cfgPtr.Store(stored)
	want := config.Clone(stored)

	resp := h.do("GET", "/api/config", nil, reqOpts{})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, secret := range []string{address, clientKey} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("GET /api/config returned a pairing secret")
		}
	}

	put := func(name string, cfg model.Config) {
		t.Helper()
		resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s PUT: want 200, got %d (%s)", name, resp.StatusCode, b)
		}
		got := h.cfgPtr.Load()
		if !reflect.DeepEqual(got.Remotes, want.Remotes) || got.RemoteSeq != want.RemoteSeq {
			t.Fatalf("%s PUT changed the stored remotes: %+v seq %d", name, got.Remotes, got.RemoteSeq)
		}
	}

	echo := h.getConfig()
	echo.DateFormat = "2006/01/02"
	put("echo", echo)
	if h.cfgPtr.Load().DateFormat != "2006/01/02" {
		t.Fatal("the PUT's own edit was not applied")
	}

	edited := h.getConfig()
	edited.Remotes[0].Name = "renamed"
	edited.Remotes[0].Address = "tc-other"
	edited.Remotes = append(edited.Remotes, model.RemoteProfile{ID: "r9", Name: "x", Address: "a", ClientKey: "k"})
	edited.RemoteSeq = 0
	put("edited", edited)

	omitted := h.getConfig()
	omitted.Remotes, omitted.RemoteSeq = nil, 0
	put("omitted", omitted)
}
