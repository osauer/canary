package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The fixture daemon has one route per way a handler can reach the IBKR
// connector. Only a read outside the seam counts, a callback's included; the
// order-write route is not counted, and building the route table reaches no
// handler.
func TestAnalyseFindsReadsOutsideTheSeam(t *testing.T) {
	got, err := analyse(filepath.Join("testdata", "fixture"), "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"direct":   {"(*Server).handleDirect"},
		"callback": {"(*Server).handleCallback$1"},
	}
	if !maps.EqualFunc(got.routes, want, slices.Equal) {
		t.Errorf("routes = %v, want %v", got.routes, want)
	}
	// handleDirect, handleCallback$1 and handleOrder read outside the seam;
	// the seam's own gatewayConnector call in brokerReads counts in calls only.
	if len(got.sites) != 3 || got.calls != 3 {
		t.Errorf("sites = %v, calls = %d", got.sites, got.calls)
	}
}

// The recorded list only shrinks: a new entry and a stale one both fail.
func TestRatchetRefusesGrowthAndStaleEntries(t *testing.T) {
	if p := ratchet("route", []string{"a", "b"}, []string{"a", "b"}); len(p) != 0 {
		t.Errorf("unchanged list refused: %v", p)
	}
	p := ratchet("route", []string{"a", "new"}, []string{"a", "gone"})
	if len(p) != 2 || !strings.Contains(p[0], "new newly reaches") || !strings.Contains(p[1], "gone no longer reaches") {
		t.Errorf("problems = %v", p)
	}
}

func TestListRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.txt")
	if err := writeList(path, []string{"r1", "r2"}, []string{"f1"}); err != nil {
		t.Fatal(err)
	}
	routes, funcs, err := readList(path)
	if err != nil || !slices.Equal(routes, []string{"r1", "r2"}) || !slices.Equal(funcs, []string{"f1"}) {
		t.Errorf("routes %v, funcs %v, err %v", routes, funcs, err)
	}
	if err := os.WriteFile(path, []byte("stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readList(path); err == nil {
		t.Error("a line before [routes] was accepted")
	}
}
