package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
)

func forwardTestServer(t *testing.T, path string, now *time.Time) *Server {
	t.Helper()
	store, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &Server{coreStore: store, now: func() time.Time { return *now }}
}

func forwardTestInstants(t *testing.T, s *Server, scope string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	for _, test := range s.edgeForwardTestsView(t.Context(), scope) {
		out[test.ID] = test.RegisteredAt
	}
	return out
}

// The instant a rule was registered is the evidence that it was written
// before the results it is read on, so nothing may move it: not a later
// rebuild, not a daemon restart on the same store, not an update that throws
// the Edge publication away, and not another account's registration. A rule
// whose text changes is registered again with its own later instant; the
// record of the old text stays, and no other rule moves.
func TestEdgeForwardTestRegistrationIsWrittenOnce(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	t0 := time.Date(2026, 11, 20, 6, 30, 0, 0, time.UTC)
	now := t0
	s := forwardTestServer(t, path, &now)
	const scope, other = "scope-a", "scope-b"

	if got := forwardTestInstants(t, s, scope); len(got) != len(edgecore.ForwardTests()) || !got["floor_after_partial_sale"].IsZero() {
		t.Fatalf("before registration=%v, want every rule listed without an instant", got)
	}
	if err := s.registerEdgeForwardTests(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	first := forwardTestInstants(t, s, scope)
	for id, at := range first {
		if !at.Equal(t0) {
			t.Fatalf("rule %s registered at %s, want %s", id, at, t0)
		}
	}

	// A later rebuild, and an update that replaces the Edge publication with
	// a newer analytical version and empties the bar cache.
	now = t0.Add(48 * time.Hour)
	for _, kind := range []string{edgePublicationStateKind, edgeBarCacheStateKind} {
		raw, err := json.Marshal(map[string]any{"version": 999})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.replaceEdgeStateDocument(t.Context(), kind, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.registerEdgeForwardTests(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	// Another account registers at its own time and leaves the first alone.
	if err := s.registerEdgeForwardTests(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if got := forwardTestInstants(t, s, other)["floor_after_partial_sale"]; !got.Equal(now) {
		t.Fatalf("other scope registered at %s, want %s", got, now)
	}

	// A restart: a new process on the same store file.
	if err := s.coreStore.Close(); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(72 * time.Hour)
	s = forwardTestServer(t, path, &now)
	if err := s.registerEdgeForwardTests(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	for id, at := range forwardTestInstants(t, s, scope) {
		if !at.Equal(first[id]) {
			t.Fatalf("rule %s moved from %s to %s", id, first[id], at)
		}
	}

	// One rule's text changes: it alone is registered again, now.
	shipped := edgeForwardTestRules
	t.Cleanup(func() { edgeForwardTestRules = shipped })
	edgeForwardTestRules = func() []edgecore.ForwardTest {
		rules := shipped()
		rules[0].Rule += " Reworded."
		return rules
	}
	changed := shipped()[0].ID
	if got := forwardTestInstants(t, s, scope)[changed]; !got.IsZero() {
		t.Fatalf("a reworded rule shows the old text's instant %s before it is registered", got)
	}
	if err := s.registerEdgeForwardTests(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	for id, at := range forwardTestInstants(t, s, scope) {
		want := first[id]
		if id == changed {
			want = now
		}
		if !at.Equal(want) {
			t.Fatalf("rule %s at %s, want %s", id, at, want)
		}
	}
	registry, _, err := s.loadEdgeForwardTestRegistry(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(registry.Scopes[scope]); got != len(shipped())+1 {
		t.Fatalf("records=%d, want the old text's record kept beside the new one", got)
	}

	// A decision made before a rule's registration is outside its sample,
	// one made after is inside; the reworded rule's sample starts later.
	view := s.edgeForwardTestsView(t.Context(), scope)
	if ids := edgeForwardSampleIDs(view, t0); len(ids) != 0 {
		t.Fatalf("a decision at the registration instant is in %v", ids)
	}
	if ids := edgeForwardSampleIDs(view, t0.Add(time.Hour)); len(ids) != len(shipped())-1 {
		t.Fatalf("an hour after registration: in %d samples, want every rule but the reworded one", len(ids))
	}
	if ids := edgeForwardSampleIDs(view, now.Add(time.Hour)); len(ids) != len(shipped()) {
		t.Fatalf("after the re-registration: in %d samples, want all", len(ids))
	}
}

// The registry is not something the daemon can fetch or derive again, so the
// store's repair path must never drop it: a damaged registry stops the
// repair instead of being replaced by a fresh one with new instants.
func TestEdgeForwardTestRegistryIsNotRebuildable(t *testing.T) {
	if rebuildableStateKinds[edgeForwardTestsStateKind] {
		t.Fatal("the forward-test registry is listed as rebuildable: a store repair would restart every rule's clock")
	}
}

// A registry written by a later build is never rewritten by this one.
func TestEdgeForwardTestRegistryFromALaterBuildIsLeftAlone(t *testing.T) {
	now := time.Date(2026, 11, 20, 6, 30, 0, 0, time.UTC)
	s := forwardTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), &now)
	raw := []byte(`{"version":99,"scopes":{"scope-a":[{"id":"x","rule_hash":"rule_x","registered_at":"2026-01-01T00:00:00Z"}]},"later":"field"}`)
	if err := s.replaceEdgeStateDocument(t.Context(), edgeForwardTestsStateKind, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.registerEdgeForwardTests(t.Context(), "scope-a"); err == nil {
		t.Fatal("a later build's registry was accepted")
	}
	doc, ok, err := s.coreStore.GetStateDocument(t.Context(), daemonStateScope, edgeForwardTestsStateKind)
	if err != nil || !ok || string(doc.JSON) != string(raw) {
		t.Fatalf("the later build's registry was changed: ok=%v err=%v json=%s", ok, err, doc.JSON)
	}
}
