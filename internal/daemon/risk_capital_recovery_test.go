package daemon

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func recoveryStore(t *testing.T) (*riskCapitalStore, *risk.Constitution, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	raw, err := json.Marshal(riskCapitalSQLiteDocument{Version: riskCapitalSQLiteDocVer, State: riskCapitalStateFileV1{Version: riskCapitalStateVer}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindRiskCapital, JSON: raw}); err != nil {
		t.Fatal(err)
	}
	st := &riskCapitalStore{now: func() time.Time { return now }}
	if err := st.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	c := testConstitutionV3()
	if _, err := st.ApplyCapitalEventForPolicyScope(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY, c, testLiveObserveScope); err != nil {
		t.Fatal(err)
	}
	st.Observe(260000, now, c, testLiveObserveScope, true)
	now = now.Add(time.Minute)
	st.Observe(240000, now, c, testLiveObserveScope, true)
	if !st.state.BlockLatched {
		t.Fatal("fixture did not breach")
	}
	return st, c, &now
}

func recoveryEvents(t *testing.T, st *riskCapitalStore) int {
	t.Helper()
	events, err := loadCoreEventsForScope(t.Context(), st.core, st.scopeKey, coreEventRiskPolicy)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, event := range events {
		var row map[string]any
		if err := json.Unmarshal(event.PayloadJSON, &row); err != nil {
			t.Fatal(err)
		}
		if row["kind"] == "drawdown_latch_recovered" {
			n++
		}
	}
	return n
}

func TestDrawdownRecoveryPersistsWithoutRebasingAndReengages(t *testing.T) {
	st, c, now := recoveryStore(t)
	peakAt, episode := st.state.PeakAsOf, st.state.LatchEpisodeSeq
	// Confirm the initial loss using the existing statement replay path.
	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{Scope: testLiveObserveScope, CoverageTo: *now}, c); err != nil {
		t.Fatal(err)
	}
	if st.state.LatchProvisional {
		t.Fatal("statement did not confirm the breach")
	}
	*now = now.Add(time.Minute)
	st.Observe(245000, *now, c, testLiveObserveScope, true)
	if !st.state.BlockLatched || recoveryEvents(t, st) != 0 {
		t.Fatal("equality with the block threshold released the brake")
	}
	*now = now.Add(time.Minute)
	st.Observe(250000, *now, c, testLiveObserveScope, true)
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.BlockLatched || rep.Tier != risk.CapitalTierWarn || *rep.ConsumedPct != 20 || st.state.AdjustedPeakBase != 260000 || !st.state.PeakAsOf.Equal(peakAt) {
		t.Fatalf("recovery lost drawdown or baseline: %+v", rep)
	}
	if open, _, _ := st.NudgeLatchForScope(testLiveObserveScope); open {
		t.Fatal("recovered brake still feeds an active drawdown nudge")
	}
	st.Observe(250000, *now, c, testLiveObserveScope, true)
	if recoveryEvents(t, st) != 1 {
		t.Fatal("recovery was lost or journaled more than once")
	}
	restarted := &riskCapitalStore{now: st.now}
	if err := restarted.bindCore(t.Context(), st.core); err != nil {
		t.Fatal(err)
	}
	if rep := restarted.Report(c, nil, testLiveObserveScope); rep.BlockLatched || rep.Tier != risk.CapitalTierWarn {
		t.Fatalf("restart resurrected the brake: %+v", rep)
	}
	*now = now.Add(time.Minute)
	restarted.Observe(240000, *now, c, testLiveObserveScope, true)
	if !restarted.state.BlockLatched || restarted.state.LatchEpisodeSeq != episode+1 || restarted.state.AdjustedPeakBase != 260000 {
		t.Fatal("a renewed breach failed to create a new brake episode on the original baseline")
	}
}

func TestDrawdownRecoveryRejectsUnprovenEvidence(t *testing.T) {
	for _, name := range []string{"stale_equity", "stale_reconcile", "unapproved", "inactive_policy", "missing", "future", "older", "nan", "infinity", "wrong_scope", "nan_capital", "nan_block", "infinite_floor"} {
		t.Run(name, func(t *testing.T) {
			st, c, now := recoveryStore(t)
			*now = now.Add(time.Minute)
			asOf, equity, allowed, scope := *now, 250000.0, true, testLiveObserveScope
			switch name {
			case "stale_equity":
				*now = now.Add(5 * time.Hour)
			case "stale_reconcile":
				*now = now.Add(8 * 24 * time.Hour)
				asOf = *now
			case "unapproved":
				c.Drawdown.WarnConsumedPct = nil
			case "inactive_policy":
				allowed = false
			case "missing":
				asOf = time.Time{}
			case "future":
				asOf = now.Add(time.Hour)
			case "older":
				asOf = now.Add(-2 * time.Minute)
			case "nan":
				equity = math.NaN()
			case "infinity":
				equity = math.Inf(1)
			case "nan_capital":
				c.Capital.DeclaredRiskCapital = new(math.NaN())
			case "nan_block":
				c.Drawdown.BlockConsumedPct = new(math.NaN())
			case "infinite_floor":
				c.Capital.ProtectedFloor = new(math.Inf(1))
			case "wrong_scope":
				scope.Mode = rpc.AccountModePaper
			}
			st.Observe(equity, asOf, c, scope, allowed)
			if !st.state.BlockLatched || recoveryEvents(t, st) != 0 {
				t.Fatal("unproven evidence released the brake")
			}
		})
	}
}

func TestDrawdownRecoveryCommitFailureKeepsBrake(t *testing.T) {
	st, c, now := recoveryStore(t)
	// Another writer advances the scoped CAS revision after this reader.
	*now = now.Add(time.Second)
	other := &riskCapitalStore{now: st.now}
	if err := other.bindCore(t.Context(), st.core); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ApplyCapitalEventForPolicyScope(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY, c, testLiveObserveScope); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	st.Observe(250000, *now, c, testLiveObserveScope, true)
	if !st.state.BlockLatched || recoveryEvents(t, st) != 0 {
		t.Fatal("failed persistence published or journaled a release")
	}
}
