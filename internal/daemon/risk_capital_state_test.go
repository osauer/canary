package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func testConstitution() *risk.Constitution {
	return &risk.Constitution{
		Kind:          risk.ConstitutionKind,
		SchemaVersion: 1,
		PolicyID:      "risk-constitution",
		PolicyVersion: 1,
		Capital: risk.ConstitutionCapital{
			BaseCurrency:        "EUR",
			ProtectedFloor:      new(200000.0),
			DeclaredRiskCapital: new(50000.0),
			MaxEquityAgeMinutes: new(240),
			MaxUnreconciledDays: new(7),
		},
		Drawdown: risk.ConstitutionDrawdown{
			WarnConsumedPct:  new(15.0),
			BlockConsumedPct: new(30.0),
		},
		Override: risk.ConstitutionOverride{MaxDurationHours: new(24)},
		Recon: risk.ConstitutionRecon{
			AmountTolerancePct:     new(0.5),
			AmountToleranceMin:     new(5.0),
			DateWindowBusinessDays: new(3),
			MaxReportAgeDays:       new(4),
		},
		Cadence: risk.ConstitutionCadence{
			Morning: risk.ConstitutionArtefact{Class: risk.EnforcementAdvisory},
		},
	}
}

func testV3Constitution() *risk.Constitution {
	c := testConstitution()
	c.PolicyVersion = 3
	c.Recon.MaxEquityDivergencePct = new(1.0)
	return c
}

func newTestRiskCapitalStore(t *testing.T) *riskCapitalStore {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return &riskCapitalStore{now: time.Now}
}

func reconcileNow(t *testing.T, st *riskCapitalStore) {
	t.Helper()
	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRiskCapitalObserveSeedsAndTracksPeak(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier = %s (%v), want ok", rep.Tier, rep.Reasons)
	}
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 260000 {
		t.Fatalf("peak = %v, want 260000", rep.AdjustedPeakBase)
	}

	st.Observe(252000, now.Add(-time.Minute), c, testLiveObserveScope) // −8k = 16% of 50k declared
	rep = st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierWarn {
		t.Fatalf("tier = %s, want warn", rep.Tier)
	}
	if rep.BlockLatched {
		t.Fatal("warn tier must not latch")
	}

	st.Observe(258000, now, c, testLiveObserveScope) // recovery: warn self-clears (decision 5)
	if rep = st.Report(c, nil, testLiveObserveScope); rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier after recovery = %s, want ok (warn is self-clearing)", rep.Tier)
	}
}

func TestRiskCapitalDailySamplesBounded(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	st.Observe(250000, now, c, testLiveObserveScope)

	now = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	st.Observe(251000, now, c, testLiveObserveScope)
	st.mu.Lock()
	st.state.DailyEquity["2026-05-01"] = 240000
	st.state.DailyEquity["not-a-day"] = 1
	st.mu.Unlock()

	now = time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	st.Observe(252000, now, c, testLiveObserveScope)
	for day, want := range map[string]float64{
		"2026-06-10": 250000,
		"2026-07-01": 251000,
		"2026-07-18": 252000,
	} {
		if got, ok := st.DailySample(day); !ok || got != want {
			t.Fatalf("sample[%s] = %v, %v; want %v, true", day, got, ok, want)
		}
	}
	for _, day := range []string{"2026-05-01", "not-a-day"} {
		if _, ok := st.DailySample(day); ok {
			t.Fatalf("expired or malformed sample %q was not pruned", day)
		}
	}

	reloaded := &riskCapitalStore{now: func() time.Time { return now }}
	ctx := reloaded.ReplayContext()
	if len(ctx.DailyEquity) != 3 || ctx.DailyEquity["2026-06-10"] != 250000 || ctx.DailyEquity["2026-07-18"] != 252000 {
		t.Fatalf("reloaded daily samples = %#v", ctx.DailyEquity)
	}
	ctx.DailyEquity["2026-07-18"] = 1
	if got, _ := reloaded.DailySample("2026-07-18"); got != 252000 {
		t.Fatalf("ReplayContext returned aliased map; stored sample changed to %v", got)
	}
}

// A declared deposit must not read as a new peak, and a declared withdrawal
// must not read as drawdown (cash-flow-adjusted HWM, decision 4).
func TestRiskCapitalExternalFlowsDoNotMoveDrawdown(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "deposit", AmountBase: 20000}, rpc.OrderOriginHumanTTY, nil); err != nil {
		t.Fatal(err)
	}
	st.Observe(280000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 260000 {
		t.Fatalf("peak after deposit = %v, want unchanged 260000", rep.AdjustedPeakBase)
	}
	if rep.Tier != risk.CapitalTierOK || (rep.ConsumedPct != nil && *rep.ConsumedPct != 0) {
		t.Fatalf("tier = %s consumed = %v, deposit must be flow-neutral", rep.Tier, rep.ConsumedPct)
	}

	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "withdrawal", AmountBase: 30000}, rpc.OrderOriginHumanTTY, nil); err != nil {
		t.Fatal(err)
	}
	st.Observe(250000, now.Add(-time.Minute), c, testLiveObserveScope)
	rep = st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier after withdrawal = %s (consumed %v), want ok — a withdrawal is not a loss", rep.Tier, rep.ConsumedPct)
	}
}

// A deposit declared late — after the peak already reflected the money —
// corrects the peak downward rather than overstating drawdown forever.
func TestRiskCapitalLateDepositCorrectsPeak(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(280000, now.Add(-time.Hour), c, testLiveObserveScope) // peak includes an undeclared 20k deposit
	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{
		Type: "deposit", AmountBase: 20000, EffectiveAt: now.Add(-2 * time.Hour),
	}, rpc.OrderOriginHumanTTY, nil); err != nil {
		t.Fatal(err)
	}
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 260000 {
		t.Fatalf("peak = %v, want corrected to 260000", rep.AdjustedPeakBase)
	}
}

func TestRiskCapitalV3NoStatementsTreatsDeclarationsAsBridge(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testV3Constitution()
	now := time.Now()
	st.now = func() time.Time { return now }
	reconcileNow(t, st)
	if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "deposit", AmountBase: 20000, EffectiveAt: now}, rpc.OrderOriginHumanTTY, c); err != nil {
		t.Fatal(err)
	}
	st.Observe(280000, now, c, testLiveObserveScope)
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.FlowSource != rpc.CapitalFlowSourceStatement || rep.DeclaredCumFlowsBase == nil || *rep.DeclaredCumFlowsBase != 20000 ||
		rep.StatementCumFlowsBase == nil || *rep.StatementCumFlowsBase != 20000 || rep.CumExternalFlowsBase == nil || *rep.CumExternalFlowsBase != 20000 {
		t.Fatalf("v3 no-statement bridge report = %+v", rep)
	}
}

func TestRiskCapitalDualComputeFieldsAreVersionGated(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	now := time.Now()
	st.mu.Lock()
	st.loadLocked()
	st.state.Seeded = true
	st.state.AdjustedPeakBase = 260000
	st.state.LastEquityBase = 260000
	st.state.LastEquityAsOf = now
	st.cumFlowsBase = 500
	st.declaredEvents = []capitalEventV1{{Version: 1, Type: "deposit", AmountBase: 500, At: now, EffectiveAt: now}}
	st.state.StatementAuthorityActive = true
	st.state.StatementFlowsBase = 700
	st.state.StatementCoverageTo = now
	st.lastReconciledAt = now
	st.mu.Unlock()
	v2 := st.Report(testConstitution(), nil, testLiveObserveScope)
	if v2.FlowSource != rpc.CapitalFlowSourceDeclared || v2.DeclaredCumFlowsBase == nil || *v2.DeclaredCumFlowsBase != 500 || v2.StatementCumFlowsBase != nil {
		t.Fatalf("v2 dual fields = %+v", v2)
	}
	v3 := st.Report(testV3Constitution(), nil, testLiveObserveScope)
	if v3.FlowSource != rpc.CapitalFlowSourceStatement || v3.DeclaredCumFlowsBase == nil || *v3.DeclaredCumFlowsBase != 500 ||
		v3.StatementCumFlowsBase == nil || *v3.StatementCumFlowsBase != 700 ||
		v3.CumExternalFlowsBase == nil || *v3.CumExternalFlowsBase != 500 {
		t.Fatalf("v3 dual fields = %+v", v3)
	}
}

func TestRiskCapitalV3StatementValueDateCorrectsPeakExactlyOnce(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	peakAt := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	st.mu.Lock()
	st.loadLocked()
	st.state.Seeded = true
	st.state.GenesisAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	st.state.AdjustedPeakBase = 280000
	st.state.PeakAsOf = peakAt
	st.mu.Unlock()

	old := reconFlow{id: "old", valueDate: time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), amountBase: 20000}
	st.IncorporateStatementSnapshot(statementCapitalSnapshot{FlowsBase: 20000, CoverageTo: peakAt, Flows: []reconFlow{old}})
	if got := st.ReplayContext().AdjustedPeakBase; got != 280000 {
		t.Fatalf("activation changed peak to %.0f", got)
	}
	newDeposit := reconFlow{id: "new", valueDate: time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC), amountBase: 10000}
	snap := statementCapitalSnapshot{FlowsBase: 30000, CoverageTo: peakAt, Flows: []reconFlow{old, newDeposit}}
	st.IncorporateStatementSnapshot(snap)
	if got := st.ReplayContext().AdjustedPeakBase; got != 270000 {
		t.Fatalf("first incorporation peak = %.0f, want 270000", got)
	}
	st.IncorporateStatementSnapshot(snap)
	if got := st.ReplayContext().AdjustedPeakBase; got != 270000 {
		t.Fatalf("rebuild corrected twice: %.0f", got)
	}
	reloaded := &riskCapitalStore{now: st.now}
	reloaded.IncorporateStatementSnapshot(snap)
	if got := reloaded.ReplayContext().AdjustedPeakBase; got != 270000 {
		t.Fatalf("restart corrected twice: %.0f", got)
	}
	reloaded.mu.Lock()
	if len(reloaded.state.AppliedStatementPeakCorrectionIDs) != 1 || reloaded.state.AppliedStatementPeakCorrectionIDs[0] != "new" {
		t.Fatalf("applied correction ids = %v", reloaded.state.AppliedStatementPeakCorrectionIDs)
	}
	reloaded.mu.Unlock()
}

func TestRiskCapitalV3PostPeakStatementAndBridgeDoNotCorrectPeak(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testV3Constitution()
	peakAt := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	st.mu.Lock()
	st.loadLocked()
	st.state.Seeded = true
	st.state.AdjustedPeakBase = 280000
	st.state.PeakAsOf = peakAt
	st.state.StatementAuthorityActive = true
	st.mu.Unlock()
	postPeak := reconFlow{id: "later", valueDate: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), amountBase: 10000}
	st.IncorporateStatementSnapshot(statementCapitalSnapshot{FlowsBase: 10000, CoverageTo: peakAt, Flows: []reconFlow{postPeak}})
	if got := st.ReplayContext().AdjustedPeakBase; got != 280000 {
		t.Fatalf("post-peak statement changed peak to %.0f", got)
	}
	if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "deposit", AmountBase: 5000, EffectiveAt: peakAt.Add(-time.Hour)}, rpc.OrderOriginHumanTTY, c); err != nil {
		t.Fatal(err)
	}
	if got := st.ReplayContext().AdjustedPeakBase; got != 280000 {
		t.Fatalf("v3 bridge declaration changed peak to %.0f", got)
	}
}

func TestRiskCapitalV3ActivationSafetyWhenFlowSumsEqual(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	v2, v3 := testConstitution(), testV3Constitution()
	now := time.Now()
	st.now = func() time.Time { return now }
	st.mu.Lock()
	st.loadLocked()
	st.state.Seeded = true
	st.state.AdjustedPeakBase = 260000
	st.state.PeakAsOf = now.Add(-time.Hour)
	st.state.LastEquityBase = 255000
	st.state.LastEquityAsOf = now
	st.state.BlockLatched = false
	st.cumFlowsBase = 1000
	st.declaredEvents = []capitalEventV1{{Version: 1, Type: "deposit", AmountBase: 1000, At: now.Add(-24 * time.Hour), EffectiveAt: now.Add(-24 * time.Hour)}}
	st.lastReconciledAt = now
	st.mu.Unlock()
	before := st.Report(v2, nil, testLiveObserveScope)
	flow := reconFlow{id: "existing", valueDate: now.Add(-24 * time.Hour), amountBase: 1000}
	st.IncorporateStatementSnapshot(statementCapitalSnapshot{FlowsBase: 1000, CoverageTo: now, Flows: []reconFlow{flow}})
	after := st.Report(v3, nil, testLiveObserveScope)
	if derefFloat(before.AdjustedPeakBase) != derefFloat(after.AdjustedPeakBase) || before.BlockLatched != after.BlockLatched ||
		before.Tier != after.Tier || derefFloat(before.ConsumedPct) != derefFloat(after.ConsumedPct) {
		t.Fatalf("activation mutated capital state: before=%+v after=%+v", before, after)
	}
}

func TestRiskCapitalV3ActivationSafetyWithZeroFlows(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	v2, v3 := testConstitution(), testV3Constitution()
	now := time.Now()
	st.mu.Lock()
	st.loadLocked()
	st.state.Seeded = true
	st.state.AdjustedPeakBase = 260000
	st.state.PeakAsOf = now.Add(-time.Hour)
	st.state.LastEquityBase = 255000
	st.state.LastEquityAsOf = now
	st.lastReconciledAt = now
	st.mu.Unlock()
	before := st.Report(v2, nil, testLiveObserveScope)
	st.IncorporateStatementSnapshot(statementCapitalSnapshot{CoverageTo: now})
	after := st.Report(v3, nil, testLiveObserveScope)
	if derefFloat(before.AdjustedPeakBase) != derefFloat(after.AdjustedPeakBase) || before.BlockLatched != after.BlockLatched ||
		before.Tier != after.Tier || derefFloat(before.ConsumedPct) != derefFloat(after.ConsumedPct) {
		t.Fatalf("zero-flow activation mutated capital state: before=%+v after=%+v", before, after)
	}
}

func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func TestRiskCapitalBlockLatchPersistsAndResets(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-2*time.Minute), c, testLiveObserveScope) // −20k = 40% ≥ block 30%
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.Tier != risk.CapitalTierBlock || !rep.BlockLatched {
		t.Fatalf("tier = %s latched = %v, want block/true", rep.Tier, rep.BlockLatched)
	}

	// Mark recovery does not clear the latch (decision 5)…
	st.Observe(262000, now.Add(-time.Minute), c, testLiveObserveScope)
	if rep = st.Report(c, nil, testLiveObserveScope); rep.Tier != risk.CapitalTierBlock {
		t.Fatalf("tier after recovery = %s, want block (latched)", rep.Tier)
	}

	// …and neither does a daemon restart: a fresh store reads it back.
	st2 := &riskCapitalStore{now: time.Now}
	if rep = st2.Report(c, nil, testLiveObserveScope); !rep.BlockLatched {
		t.Fatal("latch must survive a restart via risk-capital-state.json")
	}

	// Reset requires a reason, clears the latch, re-bases the peak.
	if err := st2.ResetDrawdown("", c); err == nil {
		t.Fatal("reset without a reason must fail")
	}
	if err := st2.ResetDrawdown("weekly review 2026-07-12: de-risked, resuming at reduced size", c); err != nil {
		t.Fatal(err)
	}
	rep = st2.Report(c, nil, testLiveObserveScope)
	if rep.BlockLatched || rep.Tier == risk.CapitalTierBlock {
		t.Fatalf("after reset: tier = %s latched = %v, want unlatched", rep.Tier, rep.BlockLatched)
	}
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 262000 {
		t.Fatalf("peak after reset = %v, want re-based to last equity 262000", rep.AdjustedPeakBase)
	}
}

// The governance journal must carry the policy fingerprint key — the gap
// rules-decisions.jsonl has — so calibration replay can prove which exact
// policy produced a transition.
func TestRiskCapitalJournalCarriesFingerprint(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()
	st.Observe(260000, now.Add(-2*time.Minute), c, testLiveObserveScope)
	st.Observe(240000, now.Add(-time.Minute), c, testLiveObserveScope)

	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sawLatch bool
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("journal line is not JSON: %q", line)
		}
		if entry["kind"] == "drawdown_block_latched" {
			sawLatch = true
			fp, _ := entry["policy_fingerprint"].(string)
			if !strings.HasPrefix(fp, "sha256:") {
				t.Fatalf("latch entry fingerprint = %q, want sha256 key", fp)
			}
		}
	}
	if !sawLatch {
		t.Fatal("no drawdown_block_latched journal entry written")
	}
}

func TestRiskCapitalOverrides(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()

	if _, err := st.GrantOverride(rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "r", Hours: 48}, c); err == nil ||
		!strings.Contains(err.Error(), "exceed override.max_duration_hours") {
		t.Fatalf("over-cap override: err = %v, want duration-cap rejection", err)
	}
	if _, err := st.GrantOverride(rpc.OverrideParams{Control: "trading.freeze", Reason: "r", Hours: 1}, c); err == nil ||
		!strings.Contains(err.Error(), "not a constitution key") {
		t.Fatalf("non-constitution control: err = %v, want rejection (invariants have no keys)", err)
	}
	if _, err := st.GrantOverride(rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "r", Hours: 1}, nil); err == nil ||
		!strings.Contains(err.Error(), "unapproved") {
		t.Fatalf("nil policy: err = %v, want unapproved-cap rejection", err)
	}

	rec, err := st.GrantOverride(rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "earnings week, accepted elevated warn", Hours: 4}, c)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Active || rec.PolicyFingerprint == "" {
		t.Fatalf("override = %+v, want active with fingerprint", rec)
	}

	// Expiry is automatic: a store whose clock has moved past ExpiresAt
	// prunes and journals the expiry.
	st.now = func() time.Time { return rec.ExpiresAt.Add(time.Minute) }
	list := st.ActiveOverrides()
	if len(list) != 1 || list[0].Active {
		t.Fatalf("overrides = %+v, want one expired record", list)
	}
}

func TestRiskCapitalUnreconciledOverrideConsumptionIsControlSpecific(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control string
		expired bool
		stale   bool
	}{
		{"active outage valve", "capital.max_unreconciled_days", false, false},
		{"expired outage valve", "capital.max_unreconciled_days", true, true},
		{"different control", "drawdown.warn_consumed_pct", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestRiskCapitalStore(t)
			c := testConstitution()
			base := time.Now().UTC()
			st.now = func() time.Time { return base.Add(-8 * 24 * time.Hour) }
			reconcileNow(t, st)
			st.now = func() time.Time { return base }
			rec, err := st.GrantOverride(rpc.OverrideParams{Control: tc.control, Reason: "statement outage", Hours: 4}, c)
			if err != nil {
				t.Fatal(err)
			}
			if tc.expired {
				st.now = func() time.Time { return rec.ExpiresAt.Add(time.Minute) }
			}
			st.mu.Lock()
			st.loadLocked()
			st.state.Seeded = true
			st.state.AdjustedPeakBase = 260000
			st.state.LastEquityBase = 260000
			st.state.LastEquityAsOf = st.now()
			st.mu.Unlock()
			rep := st.Report(c, nil, testLiveObserveScope)
			if rep.ReconcileStale != tc.stale {
				t.Fatalf("reconcile stale = %v, want %v (override %+v)", rep.ReconcileStale, tc.stale, rec)
			}
		})
	}
}

func TestRiskCapitalReplayReconcileCompatibilityAndProvenance(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	for _, ev := range []capitalEventV1{
		{Version: 1, At: base, Type: "reconcile"},
		{Version: 1, At: base.Add(time.Minute), Type: "reconcile", Origin: rpc.OrderOriginHumanTTY, ReportID: "recon-human"},
		{Version: 1, At: base.Add(2 * time.Minute), Type: "reconcile", Origin: riskCapitalAutoOrigin, ReportID: "recon-auto"},
	} {
		if err := appendCapitalEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	st.EnsureLoaded()
	if st.lastReconcileReportID != "recon-auto" || st.lastReconcileSource != rpc.ReconcileSourceAutomatic ||
		st.lastAutoExtendReportID != "recon-auto" || !st.lastAutoExtendedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("replayed provenance: last=%s/%s auto=%s/%s", st.lastReconcileReportID, st.lastReconcileSource, st.lastAutoExtendReportID, st.lastAutoExtendedAt)
	}
	for _, id := range []string{"recon-human", "recon-auto"} {
		if _, ok := st.reconciledReportIDs[id]; !ok {
			t.Fatalf("replayed report id %s missing", id)
		}
	}
}

// declares morning only
