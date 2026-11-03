package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func strp(s string) *string { return &s }

func TestHeaderEditsLookup(t *testing.T) {
	e := HeaderEdits{{Key: "OBJECT", Value: "M31", From: strp("M 31")}, {Key: "FILTER", Value: "Ha"}}
	got, ok := e.Lookup("FILTER")
	if !ok || got.Value != "Ha" || got.From != nil {
		t.Fatalf("Lookup(FILTER) = %+v, %v", got, ok)
	}
	if _, ok := e.Lookup("filter"); ok {
		t.Fatal("Lookup matched a key in another case; keys match exactly")
	}
	if _, ok := HeaderEdits(nil).Lookup("OBJECT"); ok {
		t.Fatal("Lookup on a nil overlay found an entry")
	}
}

func TestHeaderEditsSetAppendsNewKeyInFirstEditOrder(t *testing.T) {
	var e HeaderEdits
	e = e.Set("FILTER", "Ha", nil)
	e = e.Set("OBJECT", "M31", strp("M 31"))
	want := HeaderEdits{{Key: "FILTER", Value: "Ha"}, {Key: "OBJECT", Value: "M31", From: strp("M 31")}}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("Set = %+v, want %+v", e, want)
	}
}

func TestHeaderEditsSetAgainKeepsPositionAndFrom(t *testing.T) {
	e := HeaderEdits{{Key: "OBJECT", Value: "M31", From: strp("M 31")}, {Key: "FILTER", Value: "Ha"}}
	got := e.Set("OBJECT", "M32", strp("ignored"))
	want := HeaderEdits{{Key: "OBJECT", Value: "M32", From: strp("M 31")}, {Key: "FILTER", Value: "Ha"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Set = %+v, want %+v", got, want)
	}
	// A nil From recorded on the first edit (the device copy had no card) stays nil.
	got = got.Set("FILTER", "OIII", strp("ignored"))
	if f, _ := got.Lookup("FILTER"); f.Value != "OIII" || f.From != nil {
		t.Fatalf("re-edited FILTER = %+v, want value OIII with nil from", f)
	}
}

func TestHeaderEditsSetDoesNotModifyReceiver(t *testing.T) {
	e := make(HeaderEdits, 1, 4)
	e[0] = HeaderEdit{Key: "OBJECT", Value: "M31"}
	_ = e.Set("OBJECT", "M32", nil)
	_ = e.Set("FILTER", "Ha", nil)
	if e[0].Value != "M31" || len(e) != 1 || e[:2][1].Key != "" {
		t.Fatalf("receiver modified: %+v (spare capacity %+v)", e, e[:2])
	}
}

func TestHeaderEditsJSON(t *testing.T) {
	e := HeaderEdits{{Key: "OBJECT", Value: "M31", From: strp("M 31")}, {Key: "FILTER", Value: "Ha"}}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"key":"OBJECT","value":"M31","from":"M 31"},{"key":"FILTER","value":"Ha","from":null}]`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
}
