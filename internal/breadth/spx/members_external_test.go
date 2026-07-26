package spx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

// TestLoadExternalRoundTrip pins SaveExternal → LoadExternal:
// pretty-printed JSON on disk, identical bytes back out.
func TestLoadExternalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	members := makeMembers(503)
	asOf := time.Date(2026, time.May, 23, 10, 0, 0, 0, time.UTC)

	if err := SaveExternal(path, members, asOf); err != nil {
		t.Fatalf("SaveExternal: %v", err)
	}
	got, gotAsOf, ok := LoadExternal(path)
	if !ok {
		t.Fatal("LoadExternal returned not-ok on a freshly saved file")
	}
	if !slices.Equal(got, members) {
		t.Errorf("members round-trip mismatch")
	}
	if !gotAsOf.Equal(asOf) {
		t.Errorf("asOf round-trip: want %v got %v", asOf, gotAsOf)
	}
}

// TestLoadExternalMissingFile returns ok=false, no error surface
// (the daemon falls back to embedded silently in this case).
func TestLoadExternalMissingFile(t *testing.T) {
	_, _, ok := LoadExternal(filepath.Join(t.TempDir(), "missing.json"))
	if ok {
		t.Error("missing file should return not-ok")
	}
}

// TestLoadExternalCorruptJSON: invalid JSON → ok=false; daemon falls
// back. No panic, no error escalation.
func TestLoadExternalCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	_, _, ok := LoadExternal(path)
	if ok {
		t.Error("corrupt JSON should return not-ok")
	}
}

// TestLoadExternalSanityBoundRejection: count below MinMembers →
// ok=false. The runtime gate matches what the refresher checks, so a
// file written by a v0.32 daemon (no bounds at load) loaded by v0.33
// stays usable as long as the count is in band.
func TestLoadExternalSanityBoundRejection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	short := makeMembers(100)
	if err := SaveExternal(path, short, time.Now()); err != nil {
		t.Fatalf("SaveExternal: %v", err)
	}
	_, _, ok := LoadExternal(path)
	if ok {
		t.Error("file with count below MinMembers should be rejected")
	}
}

// TestLoadExternalVersionMismatch: future schema bump → ok=false,
// daemon cold-rebuilds from embedded. Mirrors the same gate pattern
// the other stores in this repo use (gamma-zero, breadth-spx).
func TestLoadExternalVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	if err := os.WriteFile(path, []byte(`{"version":99,"as_of":"2026-05-22T00:00:00Z","members":["AAA"]}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, ok := LoadExternal(path)
	if ok {
		t.Error("version mismatch should return not-ok")
	}
}

// TestMembersDefaultPath confirms the XDG-aware path resolution
// matches the documented layout: $XDG_CACHE_HOME/ibkr/spx-members/...
// or $HOME/.cache/ibkr/spx-members/... on fallback.
func TestMembersDefaultPath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg")
	t.Setenv("HOME", "/home/test")
	got, err := MembersDefaultPath()
	if err != nil {
		t.Fatalf("MembersDefaultPath: %v", err)
	}
	want := filepath.Join("/tmp/xdg", "ibkr", "spx-members", MembersFilename)
	if got != want {
		t.Errorf("MembersDefaultPath:\n  want %s\n  got  %s", want, got)
	}
}

func TestExternalMembersSQLiteColdStartContinuityAndNoLegacyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), MembersFilename)
	legacyMembers := makeMembers(503)
	legacyAsOf := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if err := SaveExternal(path, legacyMembers, legacyAsOf); err != nil {
		t.Fatalf("seed legacy members: %v", err)
	}
	legacyBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read legacy members: %v", err)
	}

	authority := openBreadthTestCoreStore(t)
	if err := UseCoreMembersStore(path, authority); err != nil {
		t.Fatalf("UseCoreMembersStore: %v", err)
	}
	if got, _, ok := LoadExternal(path); ok || got != nil {
		t.Fatalf("empty SQLite authority hydrated legacy membership: ok=%v members=%d", ok, len(got))
	}
	want := makeMembers(504)
	asOf := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if err := SaveExternal(path, want, asOf); err != nil {
		t.Fatalf("save SQLite members: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, legacyBytes) {
		t.Fatalf("legacy sp500-members.json changed after attachment: err=%v", err)
	}
	got, gotAsOf, ok := LoadExternal(path)
	if !ok || !slices.Equal(got, want) || !gotAsOf.Equal(asOf) {
		t.Fatalf("SQLite members load: count=%d as_of=%v ok=%v", len(got), gotAsOf, ok)
	}
	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: membersAuthorityScope, Source: membersObservationSource, Kind: membersObservationKind,
	})
	if err != nil || len(observations) != 1 {
		t.Fatalf("atomic members observations=%d err=%v", len(observations), err)
	}
	if !observations[0].DecisionEligible {
		t.Fatal("current members observation is not decision-eligible")
	}
	if _, ok, err := authority.GetStateDocument(context.Background(), membersAuthorityScope, membersStateKind); err != nil || !ok {
		t.Fatalf("members state missing: ok=%v err=%v", ok, err)
	}
}

func TestExternalMembersSQLiteRejectsMalformedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), MembersFilename)
	authority := openBreadthTestCoreStore(t)
	payload := []byte(`{"version":1,"as_of":"2026-07-20T12:00:00Z","source":"wikipedia","url":"https://en.wikipedia.org/wiki/List_of_S%26P_500_companies","count":500,"members":["AAPL"]}`)
	if _, err := authority.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
		ScopeKey: membersAuthorityScope, Kind: membersStateKind, JSON: payload,
	}); err != nil {
		t.Fatalf("seed malformed members authority: %v", err)
	}
	if err := UseCoreMembersStore(path, authority); err == nil {
		t.Fatal("malformed members authority attached")
	}
}
