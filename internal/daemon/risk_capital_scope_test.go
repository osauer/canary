package daemon

import (
	"strings"
	"testing"
	"time"

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
