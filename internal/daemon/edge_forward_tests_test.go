package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
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
	tests, err := s.edgeForwardTestsView(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
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
	view, err := s.edgeForwardTestsView(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
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

// A registry that cannot be read is an error, never an empty list. On
// 2026-11-20 a request that ran out of its time while the daemon was busy
// after a restart got a failed read, and the reply carried an empty list
// beside otherwise full content: "could not be read" looked like "nothing is
// registered". The reply now names no rule and says why.
func TestAnUnreadableForwardTestRegistryIsNotAnEmptyList(t *testing.T) {
	now := time.Date(2026, 11, 20, 6, 30, 0, 0, time.UTC)
	s := forwardTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), &now)
	if err := s.registerEdgeForwardTests(t.Context(), "scope-a"); err != nil {
		t.Fatal(err)
	}
	// The request's time is spent before the registry is read.
	spent, cancel := context.WithCancel(t.Context())
	cancel()
	tests, err := s.edgeForwardTestsView(spent, "scope-a")
	if err == nil || len(tests) != 0 {
		t.Fatalf("tests=%d err=%v, want the failed read as an error and no list", len(tests), err)
	}
	// With the time it needs, the same registry reads in full.
	tests, err = s.edgeForwardTestsView(t.Context(), "scope-a")
	if err != nil || len(tests) != len(edgecore.ForwardTests()) || tests[0].RegisteredAt.IsZero() {
		t.Fatalf("tests=%d err=%v, want every rule with its instant", len(tests), err)
	}
}

// The fifth US session after a decision is counted on the official
// calendar: Thanksgiving is no session and the day after it is a half day.
func TestEdgeForwardFifthSessionFollowsTheCalendar(t *testing.T) {
	ny := edgeNewYork()
	fifth := edgeFifthSessionCloses()
	at, ok := fifth(time.Date(2026, 11, 23, 11, 0, 0, 0, ny))
	if want := time.Date(2026, 12, 1, 16, 0, 0, 0, ny); !ok || !at.Equal(want) {
		t.Fatalf("fifth session close=%s ok=%v, want %s", at, ok, want)
	}
	at, ok = fifth(time.Date(2026, 11, 19, 11, 0, 0, 0, ny))
	if want := time.Date(2026, 11, 27, 13, 0, 0, 0, ny); !ok || !at.Equal(want) {
		t.Fatalf("fifth session close=%s ok=%v, want the half day %s", at, ok, want)
	}
}

// A reply counts each registered rule from the published review: only
// decisions after registration, every sale whoever sent it (which ones
// Canary's protection sent is said only while that evidence is served), a
// buy grouped by the record of the close before it, and a buy now by the
// latest record.
// A rule not registered has no sample: absent, not zero.
func TestEdgeForwardTestsAreCountedFromTheReview(t *testing.T) {
	ny := edgeNewYork()
	registered := time.Date(2026, 11, 2, 9, 0, 0, 0, ny) // a Monday
	now := time.Date(2026, 12, 18, 18, 0, 0, 0, ny)      // after the close
	s := forwardTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), &now)
	for _, stamp := range []edgeSessionStamp{
		{Session: "2026-11-03", RegimeStage: "early_warning"},
		{Session: "2026-11-04", RegimeStage: "quiet"},
		{Session: "2026-12-18", RegimeStage: "confirmed_stress", PremiumBudgetActPct: new(2.5), PremiumBudgetSet: "calm"},
	} {
		stamp.ClosedAt, stamp.ServedAt = now, now
		if err := s.appendEdgeSessionStamp(t.Context(), stamp); err != nil {
			t.Fatal(err)
		}
	}
	bar := 10.0
	scored := []edgecore.HorizonScore{{Sessions: 5, HorizonClose: &bar, CloseSource: edgecore.CloseSourceDailyBar}}
	at := func(day int) time.Time { return time.Date(2026, 11, day, 11, 0, 0, 0, ny) }
	closed := func(id string) *edgecore.PositionOutcome {
		return &edgecore.PositionOutcome{State: edgecore.OptionPositionClosed, PositionID: id, ClosedBy: "trade"}
	}
	review := edgecore.Result{AsOf: now, Changes: []edgecore.Change{
		{ID: "old", Symbol: "AAA", AssetClass: "STK", Action: edgecore.ActionExit, Direction: edgecore.DirectionLong, ExecutedAt: time.Date(2026, 10, 20, 11, 0, 0, 0, ny), Scores: scored},
		{ID: "s1", Symbol: "AAA", AssetClass: "STK", Action: edgecore.ActionTrim, Direction: edgecore.DirectionLong, ExecutedAt: at(3), Scores: scored},
		{ID: "s2", Symbol: "BBB", AssetClass: "STK", Action: edgecore.ActionExit, Direction: edgecore.DirectionLong, ExecutedAt: at(5), Scores: scored,
			ProtectionContext: edgecore.ProtectionContext{Status: "linked", ExecutionCount: 1, MatchedExecutions: 1}},
		{ID: "b1", Symbol: "CCC 270115C00050000", Underlying: "CCC", AssetClass: "OPT", Action: edgecore.ActionOpen, Direction: edgecore.DirectionLong, ExecutedAt: at(4), Position: closed("p1")},
		{ID: "b2", Symbol: "DDD 270115C00050000", Underlying: "DDD", AssetClass: "OPT", Action: edgecore.ActionOpen, Direction: edgecore.DirectionLong, ExecutedAt: at(5), Position: closed("p2")},
		{ID: "b3", Symbol: "EEE 270115C00050000", Underlying: "EEE", AssetClass: "OPT", Action: edgecore.ActionOpen, Direction: edgecore.DirectionLong, ExecutedAt: at(10), Position: closed("p3")},
	}}
	tests, err := s.edgeForwardTestsView(t.Context(), "scope-a")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*rpc.EdgeForwardTest{}
	for i := range tests {
		if tests[i].ID != "entries_vs_nearby_day" {
			tests[i].RegisteredAt = registered
		}
		byID[tests[i].ID] = &tests[i]
	}
	s.countEdgeForwardTests(t.Context(), tests, edgeForwardEvidence{result: review, protectionKnown: true})
	if test := byID["entries_vs_nearby_day"]; test.Sample != nil || test.State != rpc.EdgeForwardNotCounted || test.NotCountedReason != rpc.EdgeForwardNotRegistered {
		t.Fatalf("a test that is not registered was counted: %+v", test)
	}
	sales := byID["stock_sales_vs_nearby_day"].Sample
	if sales == nil || sales.Test.Final != 2 || sales.SentByProtection == nil || sales.SentByProtection.Final != 1 || sales.ToFill != 78 || sales.State != rpc.EdgeForwardCollecting || sales.AnswersBuilt || sales.UnitsStored || sales.WindowDays != 365 {
		t.Fatalf("stock sales sample = %+v", sales)
	}
	regime := byID["option_buys_after_regime_warning"].Sample
	groups := map[string]int{}
	for _, g := range regime.Groups {
		groups[g.ID] = g.Final
	}
	if groups["after_warning"] != 1 || groups["other"] != 1 || groups[edgecore.ForwardGroupCannotSort] != 1 {
		t.Fatalf("regime groups = %+v", regime.Groups)
	}
	if today := regime.Today; today == nil || today.Session != "2026-12-18" || today.RegimeStage != "confirmed_stress" || today.Group != "after_warning" {
		t.Fatalf("regime today = %+v", regime.Today)
	}
	if today := byID["option_buys_below_premium_line"].Sample.Today; today == nil || today.PremiumBudgetActPct == nil || *today.PremiumBudgetActPct != 2.5 || today.Group != edgecore.ForwardGroupNotSorted {
		t.Fatalf("premium line today = %+v", today)
	}
	for _, test := range tests {
		if test.Question == "" || len(test.Informs) == 0 {
			t.Fatalf("rule %s served without its facts", test.ID)
		}
	}

	// Protection evidence withheld from the reply: the sales still count,
	// and which of them Canary's protection sent is not said.
	for i := range tests {
		tests[i].Sample = nil
	}
	s.countEdgeForwardTests(t.Context(), tests, edgeForwardEvidence{result: review, protectionKnown: false})
	if sales := byID["stock_sales_vs_nearby_day"].Sample; sales.Test.Final != 2 || sales.SentByProtection != nil || sales.ToFill != 78 || sales.Expected == nil {
		t.Fatalf("withheld protection: %+v", sales)
	}
}
