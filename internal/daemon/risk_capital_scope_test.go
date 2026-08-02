package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// testLiveObserveScope is the concrete live identity existing capital tests
// observe under; the scope gate adopts it on first use.
var testLiveObserveScope = brokerStateScope{Account: "U111", Mode: rpc.AccountModeLive}

// The 2026-07-19 incident: a paper-pinned daemon sharing the production state
// dir ratcheted the live peak with the paper account's ~1M equity. Any
// observation from a non-live mode, an unresolved scope, or a different
// account must be refused without touching the peak.
func TestObserveRefusesWrongScopeAndBindsAccount(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }

	st.Observe(245000, now.Add(-4*24*time.Hour), nil, testLiveObserveScope)
	if st.state.AccountID != testLiveObserveScope.Account || st.state.AccountMode != rpc.AccountModeLive {
		t.Fatalf("first live observation must bind the account: %+v", st.state)
	}
	if st.state.AdjustedPeakBase != 245000 {
		t.Fatalf("peak=%v", st.state.AdjustedPeakBase)
	}

	paper := brokerStateScope{Account: "DU333", Mode: rpc.AccountModePaper}
	st.Observe(1025033.32, now.Add(-2*time.Hour), nil, paper)
	otherLive := brokerStateScope{Account: "U222", Mode: rpc.AccountModeLive}
	st.Observe(999999, now.Add(-time.Hour), nil, otherLive)
	st.Observe(888888, now.Add(-time.Hour), nil, brokerStateScope{})
	if st.state.AdjustedPeakBase != 245000 || st.state.AccountID != testLiveObserveScope.Account {
		t.Fatalf("out-of-scope observations must never ratchet the peak: %+v", st.state)
	}
	if st.state.LastEquityBase == 1025033.32 || st.state.LastEquityBase == 999999 {
		t.Fatalf("out-of-scope observations must not update equity either: %+v", st.state)
	}

	st.Observe(230000, now, nil, testLiveObserveScope)
	if st.state.LastEquityBase != 230000 || st.state.AdjustedPeakBase != 245000 {
		t.Fatalf("matching live scope must keep observing normally: %+v", st.state)
	}
}

func TestObserveScopeRejectionReasons(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	st.state.AccountID = "U111"
	for _, tt := range []struct {
		scope brokerStateScope
		want  string
	}{
		{brokerStateScope{}, "scope_unresolved"},
		{brokerStateScope{Account: "All", Mode: rpc.AccountModeLive}, "scope_unresolved"},
		{brokerStateScope{Account: "DU333", Mode: rpc.AccountModePaper}, "non_live_mode"},
		{brokerStateScope{Account: "U222", Mode: rpc.AccountModeLive}, "account_mismatch"},
		{brokerStateScope{Account: "U111", Mode: rpc.AccountModeLive}, ""},
	} {
		if got := st.observationScopeRejectionLocked(tt.scope); got != tt.want {
			t.Fatalf("scope %+v: rejection=%q want %q", tt.scope, got, tt.want)
		}
	}
}

// CorrectPeak is the surgical repair for a poisoned peak: lower-only,
// journaled, and the latch — which recorded a real engagement — is untouched.
func TestCorrectPeakLowersOnlyAndKeepsLatch(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	st.Observe(245000, now.Add(-4*24*time.Hour), nil, testLiveObserveScope)
	st.state.AdjustedPeakBase = 1025033.32 // the poisoned ratchet
	st.state.BlockLatched = true
	st.state.LatchedAt = now.Add(-4 * 24 * time.Hour)
	st.state.LatchConsumedPct = 30.41

	if _, err := st.CorrectPeak(245380, time.Time{}, "manual", "", nil); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("missing reason must refuse: %v", err)
	}
	if _, err := st.CorrectPeak(2_000_000, time.Time{}, "manual", "raise", nil); err == nil || !strings.Contains(err.Error(), "must lower") {
		t.Fatalf("raising must refuse: %v", err)
	}

	anchor := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	from, err := st.CorrectPeak(245380, anchor, "statement_replay", "paper-account observation poisoned the peak", nil)
	if err != nil || from != 1025033.32 {
		t.Fatalf("correction failed: from=%v err=%v", from, err)
	}
	if st.state.AdjustedPeakBase != 245380 || !st.state.PeakAsOf.Equal(anchor) {
		t.Fatalf("corrected state=%+v", st.state)
	}
	if !st.state.BlockLatched || st.state.LatchConsumedPct != 30.41 {
		t.Fatalf("the latch must be untouched by a peak correction: %+v", st.state)
	}

	fresh := newTestRiskCapitalStore(t)
	if _, err := fresh.CorrectPeak(100, time.Time{}, "manual", "r", nil); err == nil || !strings.Contains(err.Error(), "not seeded") {
		t.Fatalf("unseeded correction must refuse: %v", err)
	}
}

// Audit A1's read side: the ladder is welded to the first live account it
// saw, and Report used to evaluate whatever fresh equity the caller handed
// in against that document's peak. Repinned to a smaller sibling account
// that fabricated a block tier; to a larger one it collapsed a real
// drawdown to ok. A mismatched session now reads Tier unknown with the
// binding disclosed, and never a tier computed across accounts.
func TestReportRefusesMismatchedScope(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()
	st.Observe(260000, now.Add(-time.Minute), c, testLiveObserveScope) // binds U111, peak 260k

	// Matched scope: the normal report, with the binding disclosed.
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierOK {
		t.Fatalf("matched-scope tier = %s (%v), want ok", rep.Tier, rep.Reasons)
	}
	if rep.BoundAccount != testLiveObserveScope.Account {
		t.Fatalf("bound account not disclosed on the normal report: %+v", rep)
	}

	// Mismatched account, no fresh equity: unknown, named, attributed.
	other := brokerStateScope{Account: "U222", Mode: rpc.AccountModeLive}
	rep = st.Report(c, nil, other)
	if rep.Tier != risk.CapitalTierUnknown {
		t.Fatalf("mismatched-scope tier = %s, want unknown", rep.Tier)
	}
	if rep.BoundAccount != testLiveObserveScope.Account {
		t.Fatalf("mismatched-scope report must name the binding: %+v", rep)
	}
	var named bool
	for _, r := range rep.Reasons {
		if strings.Contains(r, "bound to another account") {
			named = true
		}
	}
	if !named {
		t.Fatalf("mismatched-scope report must carry the named reason, got %v", rep.Reasons)
	}
	if rep.ConsumedPct != nil || rep.DrawdownBase != nil || rep.EquityBase != nil || rep.AdjustedPeakBase != nil {
		t.Fatalf("mismatched-scope report leaked computed magnitudes: %+v", rep)
	}

	// The wrong-number scenario: peak adopted from U111, equity handed in
	// from U222. A 200k sibling equity against the 260k peak is a 120%%
	// consumed drawdown — a fabricated block pre-fix; a 400k sibling equity
	// collapsed the tier to ok. Both must now be unknown.
	small := &risk.CapitalObservation{EquityBase: 200000, AsOf: now}
	if rep := st.Report(c, small, other); rep.Tier != risk.CapitalTierUnknown {
		t.Fatalf("sibling equity below peak fabricated tier %s, want unknown", rep.Tier)
	}
	large := &risk.CapitalObservation{EquityBase: 400000, AsOf: now}
	if rep := st.Report(c, large, other); rep.Tier != risk.CapitalTierUnknown {
		t.Fatalf("sibling equity above peak collapsed tier to %s, want unknown", rep.Tier)
	}

	// Same account, different mode: refused the same way.
	paper := brokerStateScope{Account: testLiveObserveScope.Account, Mode: rpc.AccountModePaper}
	if rep := st.Report(c, nil, paper); rep.Tier != risk.CapitalTierUnknown {
		t.Fatalf("mode-mismatched tier = %s, want unknown", rep.Tier)
	}

	// An unresolved scope still serves: with no session identity the report
	// pairs the document's own persisted equity with its own peak. Offline
	// reads stay honest rather than going dark.
	if rep := st.Report(c, nil, brokerStateScope{}); rep.Tier != risk.CapitalTierOK {
		t.Fatalf("unresolved-scope tier = %s (%v), want ok from persisted self-consistent state", rep.Tier, rep.Reasons)
	}
	// But a fresh observation without a resolvable identity is unattributable
	// and must not be evaluated against the peak; the persisted equity serves.
	if rep := st.Report(c, small, brokerStateScope{}); rep.Tier != risk.CapitalTierOK {
		t.Fatalf("unattributable fresh equity moved the tier to %s, want ok from persisted state", rep.Tier)
	}

	// A latched block stays visible across a repin, attributed to its owner —
	// hiding an engaged latch behind a pin change would be the quiet failure.
	st.mu.Lock()
	st.state.BlockLatched = true
	st.state.LatchedAt = now
	st.state.LatchConsumedPct = 31.5
	st.mu.Unlock()
	rep = st.Report(c, nil, other)
	if rep.Tier != risk.CapitalTierUnknown || !rep.BlockLatched || rep.BoundAccount != testLiveObserveScope.Account {
		t.Fatalf("latch visibility across a repin: %+v", rep)
	}
}
