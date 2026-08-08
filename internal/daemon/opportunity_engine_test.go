package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestOptionExerciseOpportunityCallUsesExecutableClose(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 103.0, 103.20, 2.0
	row := opportunityTestOption(now, "C", 100, &optionBid)
	stock := opportunityTestStock(now, -100, &bid, &ask)

	opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, now)
	if !ok {
		t.Fatal("call exercise opportunity missing")
	}
	if len(opp.Blockers) != 0 {
		t.Fatalf("blockers=%+v, want none", opp.Blockers)
	}
	if opp.PositionEffect != rpc.ExercisePositionEffectClose {
		t.Fatalf("position effect=%q, want close", opp.PositionEffect)
	}
	if opp.UnderlyingQuantityBefore != -100 || opp.UnderlyingQuantityAfter != 0 || opp.UnderlyingShareChange != 100 {
		t.Fatalf("underlying effect before/after/change = %.0f/%.0f/%.0f", opp.UnderlyingQuantityBefore, opp.UnderlyingQuantityAfter, opp.UnderlyingShareChange)
	}
	if opp.IntrinsicValue != 300 || opp.CloseValue != 200 || opp.ExpectedGain != 100 {
		t.Fatalf("economics intrinsic=%.2f close=%.2f gain=%.2f, want 300/200/100", opp.IntrinsicValue, opp.CloseValue, opp.ExpectedGain)
	}
	if opp.Reason != "exercise value exceeds executable option close value" {
		t.Fatalf("reason=%q, want positive-gain exercise wording", opp.Reason)
	}
}

func TestOptionExerciseOpportunityPutUsesUnderlyingAsk(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 101.80, 102.0, 2.5
	row := opportunityTestOption(now, "P", 105, &optionBid)
	stock := opportunityTestStock(now, 100, &bid, &ask)

	opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, now)
	if !ok {
		t.Fatal("put exercise opportunity missing")
	}
	if len(opp.Blockers) != 0 {
		t.Fatalf("blockers=%+v, want none", opp.Blockers)
	}
	if opp.PositionEffect != rpc.ExercisePositionEffectClose {
		t.Fatalf("position effect=%q, want close", opp.PositionEffect)
	}
	if opp.IntrinsicValue != 300 || opp.CloseValue != 250 || opp.ExpectedGain != 50 {
		t.Fatalf("economics intrinsic=%.2f close=%.2f gain=%.2f, want 300/250/50", opp.IntrinsicValue, opp.CloseValue, opp.ExpectedGain)
	}
}

// The flat "stocks" slice also contains bonds, futures, and indices. A
// same-symbol non-equity row must never replace the actual equity underlying
// used for option-exercise economics or position-effect classification.
func TestOpportunityGenerateUsesEquityUnderlyingWhenSymbolIsShared(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 103.0, 103.20, 2.0
	option := opportunityTestOption(now, "C", 100, &optionBid)
	stock := opportunityTestStock(now, -100, &bid, &ask)
	stock.ConID = 9001
	bond := rpc.PositionView{
		Symbol:     "AAPL",
		SecType:    "BOND",
		ConID:      9002,
		Currency:   "USD",
		Quantity:   -5,
		Multiplier: 1,
		Bid:        &bid,
		Ask:        &ask,
		PriceAt:    now,
	}
	engine := &opportunityEngine{}

	for _, tc := range []struct {
		name   string
		stocks []rpc.PositionView
	}{
		{name: "equity follows bond", stocks: []rpc.PositionView{bond, stock}},
		{name: "bond follows equity", stocks: []rpc.PositionView{stock, bond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pos := &rpc.PositionsResult{
				Stocks:  tc.stocks,
				Options: []rpc.PositionView{option},
			}
			got := engine.generate(policy, status, pos, rpc.OpportunitySourceFingerprints{}, brokerStateScope{}, now)
			if len(got) != 1 {
				t.Fatalf("opportunities=%d, want 1 using the equity underlying", len(got))
			}
			opp := got[0]
			if len(opp.Blockers) != 0 {
				t.Fatalf("blockers=%+v, want equity-underlying opportunity to remain unblocked", opp.Blockers)
			}
			if opp.UnderlyingContract.ConID != stock.ConID {
				t.Fatalf("underlying con_id=%d, want equity con_id=%d", opp.UnderlyingContract.ConID, stock.ConID)
			}
			if opp.UnderlyingQuantityBefore != stock.Quantity {
				t.Fatalf("underlying quantity before=%.0f, want equity quantity %.0f", opp.UnderlyingQuantityBefore, stock.Quantity)
			}
		})
	}
}

func TestOptionExercisePostExerciseRiskContext(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 103.0, 103.20, 2.0

	tests := []struct {
		name       string
		right      string
		stockQty   float64
		coverage   rpc.ProtectionCoverageRow
		wantEffect string
		wantChange string
		wantReview bool
		wantOpened bool
		wantIncr   bool
		wantFlip   bool
	}{
		{
			name:       "call closes short stock and needs no protection review",
			right:      "C",
			stockQty:   -100,
			wantEffect: rpc.ExercisePositionEffectClose,
			wantChange: rpc.ExerciseRiskChangeClosed,
		},
		{
			name:       "call increases long stock and needs protection review",
			right:      "C",
			stockQty:   100,
			coverage:   rpc.ProtectionCoverageRow{Underlying: "AAPL", State: rpc.ProtectionCoverageStateCovered, PositionQuantity: 100, ProtectedQuantity: 100},
			wantEffect: rpc.ExercisePositionEffectIncrease,
			wantChange: rpc.ExerciseRiskChangeIncreased,
			wantReview: true,
			wantIncr:   true,
		},
		{
			name:       "call flips short stock and needs protection review",
			right:      "C",
			stockQty:   -50,
			wantEffect: rpc.ExercisePositionEffectFlip,
			wantChange: rpc.ExerciseRiskChangeFlipped,
			wantReview: true,
			wantFlip:   true,
		},
		{
			name:       "put opens short stock and needs protection review",
			right:      "P",
			stockQty:   0,
			wantEffect: rpc.ExercisePositionEffectOpen,
			wantChange: rpc.ExerciseRiskChangeOpened,
			wantReview: true,
			wantOpened: true,
		},
		{
			name:       "stale protective order forces review",
			right:      "C",
			stockQty:   -100,
			coverage:   rpc.ProtectionCoverageRow{Underlying: "AAPL", State: rpc.ProtectionCoverageStateReconcileRequired, Orders: []rpc.ProtectionCoverageOrder{{Symbol: "AAPL", OrderType: rpc.OrderTypeTRAIL, Remaining: 100}}},
			wantEffect: rpc.ExercisePositionEffectClose,
			wantChange: rpc.ExerciseRiskChangeClosed,
			wantReview: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			right := tc.right
			strike := 100.0
			if right == "P" {
				strike = 105
			}
			optionBidForCase := optionBid
			if right == "P" {
				optionBidForCase = 1.0
			}
			row := opportunityTestOption(now, right, strike, &optionBidForCase)
			stock := opportunityTestStock(now, tc.stockQty, &bid, &ask)
			opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, now, tc.coverage)
			if !ok {
				t.Fatal("expected opportunity")
			}
			risk := opp.PostExerciseRisk
			if risk == nil {
				t.Fatal("post exercise risk context missing")
			}
			if risk.PositionEffect != tc.wantEffect || risk.RiskChange != tc.wantChange || risk.ProtectionReviewNeeded != tc.wantReview {
				t.Fatalf("risk context = %+v, want effect=%s change=%s review=%v", risk, tc.wantEffect, tc.wantChange, tc.wantReview)
			}
			if risk.RiskOpened != tc.wantOpened || risk.RiskIncreased != tc.wantIncr || risk.RiskFlipped != tc.wantFlip {
				t.Fatalf("risk booleans = opened:%v increased:%v flipped:%v", risk.RiskOpened, risk.RiskIncreased, risk.RiskFlipped)
			}
			if risk.BeforeQuantity != tc.stockQty || risk.AfterQuantity != opp.UnderlyingQuantityAfter || risk.ShareChange != opp.UnderlyingShareChange {
				t.Fatalf("risk before/after/change = %.0f/%.0f/%.0f, opportunity = %.0f/%.0f/%.0f",
					risk.BeforeQuantity, risk.AfterQuantity, risk.ShareChange,
					opp.UnderlyingQuantityBefore, opp.UnderlyingQuantityAfter, opp.UnderlyingShareChange)
			}
		})
	}
}

func TestOptionExerciseOpportunityNegativeGainReasonDoesNotOverstate(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	bid, ask, optionBid := 103.0, 103.20, 4.0
	row := opportunityTestOption(now, "C", 100, &optionBid)
	stock := opportunityTestStock(now, -100, &bid, &ask)

	opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, now)
	if ok {
		t.Fatalf("negative-gain row surfaced as opportunity: %+v", opp)
	}
}

func TestOptionExerciseOpportunityBlockersFailClosed(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	bid, ask, optionBid := 103.0, 103.20, 2.0

	tests := []struct {
		name       string
		policy     func(opportunityPolicy) opportunityPolicy
		row        func(rpc.PositionView) rpc.PositionView
		stock      func(rpc.PositionView) rpc.PositionView
		at         time.Time
		wantCode   string
		wantEffect string
		wantSkip   bool
	}{
		{
			name: "missing option bid is not a candidate",
			policy: func(p opportunityPolicy) opportunityPolicy {
				p.Buckets.OptionExercise.AllowNoOptionBid = true
				return p
			},
			row:      func(r rpc.PositionView) rpc.PositionView { r.OptionBid = nil; return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { return s },
			at:       now,
			wantSkip: true,
		},
		{
			name:     "stale option quote",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { r.PriceAt = now.Add(-time.Minute); return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { return s },
			at:       now,
			wantCode: "option_quote_stale",
		},
		{
			name:     "outside RTH",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { return s },
			at:       time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC),
			wantCode: "options_rth_required",
		},
		{
			name:     "unsupported style",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { s.SecType = rpc.SecTypeIndex; return s },
			at:       now,
			wantCode: "exercise_style_unknown_or_unsupported",
		},
		{
			name:     "underlying NBBO missing",
			policy:   func(p opportunityPolicy) opportunityPolicy { return p },
			row:      func(r rpc.PositionView) rpc.PositionView { return r },
			stock:    func(s rpc.PositionView) rpc.PositionView { s.Bid = nil; return s },
			at:       now,
			wantSkip: true,
		},
		{
			name:       "exercise can increase stock exposure",
			policy:     func(p opportunityPolicy) opportunityPolicy { return p },
			row:        func(r rpc.PositionView) rpc.PositionView { return r },
			stock:      func(s rpc.PositionView) rpc.PositionView { s.Quantity = 100; return s },
			at:         now,
			wantCode:   "exercise_not_reduce_only",
			wantEffect: rpc.ExercisePositionEffectIncrease,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := tc.policy(defaultOpportunityPolicy())
			status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", tc.at)
			row := tc.row(opportunityTestOption(tc.at, "C", 100, &optionBid))
			stock := tc.stock(opportunityTestStock(tc.at, -100, &bid, &ask))

			opp, ok := optionExerciseOpportunity(policy, status, row, stock, rpc.OpportunitySourceFingerprints{}, tc.at)
			if tc.wantSkip {
				if ok {
					t.Fatalf("row surfaced as opportunity: %+v", opp)
				}
				return
			}
			if !ok {
				t.Fatal("expected opportunity to be surfaced")
			}
			if tc.wantCode == "" {
				if len(opp.Blockers) != 0 {
					t.Fatalf("blockers=%+v, want none", opp.Blockers)
				}
				if opp.State != rpc.OpportunityStateGenerated {
					t.Fatalf("state=%q, want generated", opp.State)
				}
			} else {
				if !hasBlocker(opp.Blockers, tc.wantCode) {
					t.Fatalf("blockers=%+v, want %q", opp.Blockers, tc.wantCode)
				}
				if opp.State != rpc.OpportunityStateBlocked {
					t.Fatalf("state=%q, want blocked", opp.State)
				}
			}
			if tc.wantEffect != "" && opp.PositionEffect != tc.wantEffect {
				t.Fatalf("position effect=%q, want %q", opp.PositionEffect, tc.wantEffect)
			}
		})
	}
}

func TestClassifyExercisePositionEffect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		before float64
		after  float64
		want   string
	}{
		{before: 0, after: 100, want: rpc.ExercisePositionEffectOpen},
		{before: -100, after: 0, want: rpc.ExercisePositionEffectClose},
		{before: -200, after: -100, want: rpc.ExercisePositionEffectReduce},
		{before: 100, after: 200, want: rpc.ExercisePositionEffectIncrease},
		{before: -100, after: 100, want: rpc.ExercisePositionEffectFlip},
		{before: 100, after: 100, want: rpc.ExercisePositionEffectUnknown},
	}
	for _, tc := range tests {
		if got := classifyExercisePositionEffect(tc.before, tc.after); got != tc.want {
			t.Fatalf("classifyExercisePositionEffect(%v, %v)=%q, want %q", tc.before, tc.after, got, tc.want)
		}
	}
}

func opportunityTestRTH() time.Time {
	return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
}

func opportunityTestOption(now time.Time, right string, strike float64, bid *float64) rpc.PositionView {
	spot := 103.10
	return rpc.PositionView{
		Symbol:       "AAPL",
		SecType:      rpc.SecTypeOption,
		ConID:        12345,
		Exchange:     "SMART",
		Currency:     "USD",
		LocalSymbol:  "AAPL  260619C00100000",
		TradingClass: "AAPL",
		Quantity:     1,
		Multiplier:   100,
		PriceAt:      now,
		Expiry:       "20260619",
		Strike:       strike,
		Right:        strings.ToUpper(right),
		OptionBid:    bid,
		Underlying:   &spot,
	}
}

func opportunityTestStock(now time.Time, quantity float64, bid, ask *float64) rpc.PositionView {
	return rpc.PositionView{
		Symbol:     "AAPL",
		SecType:    rpc.SecTypeStock,
		Currency:   "USD",
		Quantity:   quantity,
		Multiplier: 1,
		Bid:        bid,
		Ask:        ask,
		PriceAt:    now,
	}
}

func opportunityTestGoodSnapshot(now time.Time) rpc.OpportunitySnapshot {
	return rpc.OpportunitySnapshot{
		Kind:          rpc.OpportunitySnapshotKind,
		SchemaVersion: rpc.OpportunitySnapshotSchemaVersion,
		AsOf:          now,
		Revision:      "sha256:last-good",
		AccountID:     "U1234567",
		AccountMode:   "live",
		PolicyID:      "opportunity-option-exercise-mvp",
		PolicyVersion: 1,
		Opportunities: []rpc.Opportunity{{Key: "option_exercise:test", Revision: "sha256:last-good"}},
	}
}

// The scheduled Run loop must record each refresh outcome exactly once
// (Refresh notes internally): a second noteRefreshOutcome per cycle
// double-counted the streak, so "blocked 4 consecutive times" meant two
// boot-race attempts and the warn threshold halved.
func TestOpportunityNoteRefreshOutcomeStreak(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	clock := base
	e := &opportunityEngine{now: func() time.Time { return clock }}
	blocked := rpc.OpportunitySnapshot{AsOf: base, Blockers: []rpc.TradingBlocker{{Code: "account_unavailable", Message: "gateway down"}}}
	for i := range 3 {
		clock = base.Add(time.Duration(i) * 30 * time.Second)
		e.noteRefreshOutcome(blocked, nil)
		if h := e.RefreshHealth(); h.Streak != i+1 {
			t.Fatalf("streak=%d after %d failed refreshes, want %d", h.Streak, i+1, i+1)
		}
	}
	h := e.RefreshHealth()
	if !h.Since.Equal(base) {
		t.Fatalf("since=%s, want first failure time %s", h.Since, base)
	}
	if len(h.Codes) != 1 || h.Codes[0] != "account_unavailable" {
		t.Fatalf("codes=%v, want [account_unavailable]", h.Codes)
	}
	e.noteRefreshOutcome(rpc.OpportunitySnapshot{AsOf: clock}, nil)
	if h := e.RefreshHealth(); h.Streak != 0 || !h.Since.IsZero() || len(h.Codes) != 0 {
		t.Fatalf("clean refresh did not reset streak: %+v", h)
	}
	e.noteRefreshOutcome(rpc.OpportunitySnapshot{}, errors.New("rpc timeout"))
	if h := e.RefreshHealth(); h.Streak != 1 || len(h.Codes) != 1 || h.Codes[0] != "rpc timeout" {
		t.Fatalf("bare error not counted with its message as code: %+v", h)
	}
	disabled := rpc.OpportunitySnapshot{Blockers: []rpc.TradingBlocker{{Code: "opportunities_disabled", Message: "disabled by config"}}}
	e.noteRefreshOutcome(disabled, nil)
	if h := e.RefreshHealth(); h.Streak != 0 {
		t.Fatalf("operator-owned disabled blocker counted as refresh failure: streak=%d", h.Streak)
	}
}

// TestOpportunityRefreshBackoffCap pins the Run-loop retry pacing at the
// opportunity engine's 2m cadence: sustained transient failures retry at 30s
// doubling up to opportunityRefreshBackoffCap (15m), NOT once per 2m cadence
// forever. A weekend gateway outage otherwise warned ~30×/hour where the
// proposals engine, already capped, warned ~4×/hour for the same outage.
func TestOpportunityRefreshBackoffCap(t *testing.T) {
	t.Parallel()
	cadence := 2 * time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, cadence},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, opportunityRefreshBackoffCap},   // 16m, capped at 15m
		{200, opportunityRefreshBackoffCap}, // shift-overflow guard
	}
	for _, tc := range cases {
		got := refreshBackoff(cadence, opportunityRefreshRetryBase, opportunityRefreshBackoffCap, tc.failures)
		if got != tc.want {
			t.Errorf("refreshBackoff(%v, %d) = %v, want %v", cadence, tc.failures, got, tc.want)
		}
	}
}

func TestOpportunityRefreshTransientCodes(t *testing.T) {
	t.Parallel()
	transient := []string{"opportunity_scope_unavailable", "opportunity_scope_mismatch", "account_unavailable", "positions_unavailable", "positions_pending"}
	for _, code := range transient {
		snap := rpc.OpportunitySnapshot{Blockers: []rpc.TradingBlocker{{Code: code}}}
		if !opportunityRefreshTransient(snap) {
			t.Errorf("code %q not classified transient", code)
		}
	}
	for _, code := range []string{"opportunities_disabled", "policy_drift", ""} {
		snap := rpc.OpportunitySnapshot{Blockers: []rpc.TradingBlocker{{Code: code}}}
		if opportunityRefreshTransient(snap) {
			t.Errorf("code %q classified transient, want operator-owned", code)
		}
	}
}

func TestOpportunityPreserveSnapshotOnRefreshFailure(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	policy := defaultOpportunityPolicy()
	policyStatus := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusDefault, "test", "", now)
	scope := brokerStateScope{Account: "U1234567", Mode: "live"}
	blockers := []rpc.TradingBlocker{{Code: "account_unavailable", Message: "gateway down"}}

	e := &opportunityEngine{snapshot: opportunityTestGoodSnapshot(now)}
	snap, ok := e.preserveSnapshotOnRefreshFailure(scope, rpc.OpportunityStatus{}, policyStatus, blockers, false)
	if !ok {
		t.Fatal("same-scope same-policy snapshot not preserved")
	}
	if snap.Revision != "sha256:last-good" || len(snap.Opportunities) != 1 {
		t.Fatalf("preserved snapshot lost content: revision=%q items=%d", snap.Revision, len(snap.Opportunities))
	}
	if len(snap.Blockers) != 1 || snap.Blockers[0].Code != "account_unavailable" {
		t.Fatalf("transient blocker not disclosed on preserved snapshot: %+v", snap.Blockers)
	}
	if served := e.Snapshot(false); served.Revision != "sha256:last-good" || len(served.Blockers) != 1 {
		t.Fatalf("served snapshot not the preserved copy: revision=%q blockers=%d", served.Revision, len(served.Blockers))
	}

	e = &opportunityEngine{snapshot: opportunityTestGoodSnapshot(now)}
	if _, ok := e.preserveSnapshotOnRefreshFailure(brokerStateScope{Account: "U7654321", Mode: "live"}, rpc.OpportunityStatus{}, policyStatus, blockers, false); ok {
		t.Fatal("preserved a snapshot across an account switch")
	}
	e = &opportunityEngine{snapshot: opportunityTestGoodSnapshot(now)}
	if _, ok := e.preserveSnapshotOnRefreshFailure(brokerStateScope{Account: "U1234567", Mode: "paper"}, rpc.OpportunityStatus{}, policyStatus, blockers, false); ok {
		t.Fatal("preserved a snapshot across a live/paper mode switch")
	}

	stale := opportunityTestGoodSnapshot(now)
	stale.PolicyVersion = 2
	e = &opportunityEngine{snapshot: stale}
	if _, ok := e.preserveSnapshotOnRefreshFailure(scope, rpc.OpportunityStatus{}, policyStatus, blockers, false); ok {
		t.Fatal("preserved a snapshot generated under a different policy version")
	}

	empty := opportunityTestGoodSnapshot(now)
	empty.Opportunities = nil
	e = &opportunityEngine{snapshot: empty}
	if _, ok := e.preserveSnapshotOnRefreshFailure(scope, rpc.OpportunityStatus{}, policyStatus, blockers, false); ok {
		t.Fatal("preserved an opportunity-free snapshot; shell regeneration is cheaper and equivalent")
	}
	e = &opportunityEngine{}
	if _, ok := e.preserveSnapshotOnRefreshFailure(scope, rpc.OpportunityStatus{}, policyStatus, blockers, false); ok {
		t.Fatal("preserved a zero-value snapshot")
	}
}

// The degraded status row reports the as_of of the snapshot actually being
// served (adopted-from-disk or preserved), not last-success bookkeeping
// that formatted as 0001-01-01T00:00:00Z on every boot race.
func TestOpportunityRefreshHealthServedAsOf(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	e := &opportunityEngine{snapshot: opportunityTestGoodSnapshot(now)}
	if h := e.RefreshHealth(); !h.ServedAsOf.Equal(now) {
		t.Fatalf("served as_of=%s, want adopted snapshot time %s", h.ServedAsOf, now)
	}
}
