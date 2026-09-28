package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Owner decisions of 2026-09-28:
//   - "Option exits in the last 14 days: the loss exit keeps working until
//     expiry, and a new proposal closes in-the-money long options before
//     expiry, so nothing is exercised by accident. The profit trail keeps its
//     14-day floor."
//   - "Profit trail on single options: keep it armed from the peak, carrying a
//     high-water mark the way units do."

// expiryCallFixture holds one standing directional long call on an ordinary
// underlying (two contracts, strike 100, cost 1.00 per share) beside a long
// stock, dte calendar days from expiry. The complete book evidence carries the
// call's exact model receipt with the given underlying price; the call's exact
// quote is bid/ask.
func expiryCallFixture(t *testing.T, dte int, underlying, bid, ask float64) (*proposalEngine, *optionEvidenceFixture, *rpc.PositionsResult, time.Time) {
	t.Helper()
	f, pos, now := newOptionEvidenceFixture()
	row := optionExitTestRow()
	row.Expiry = now.AddDate(0, 0, dte).Format("20060102")
	row.LocalSymbol = fmt.Sprintf("TEST  %sC00100000", row.Expiry[2:])
	pos.Options[0] = row
	contract := *previewIBKRContract(proposalContractFromPosition(row, "OPT"))
	f.scope.Positions[1].Contract = contract
	f.models[row.ConID].Contract = contract
	f.models[row.ConID].Delta = new(0.5)
	f.models[row.ConID].Underlying = new(underlying)
	f.prices = map[int]rpc.OrderQuoteSnapshot{row.ConID: unitQuote(now, bid, ask)}
	engine := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	return engine, f, pos, now
}

func generateOptionExits(t *testing.T, engine *proposalEngine, pos *rpc.PositionsResult, now time.Time) []rpc.TradeProposal {
	t.Helper()
	proposals, _ := engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	return proposals
}

// hermeticOptionExitBlockersOnly reports whether the only blocker is the
// missing broker open-order inventory, which a hermetic test cannot supply;
// an action row never skips that check.
func hermeticOptionExitBlockersOnly(p rpc.TradeProposal) bool {
	return len(p.Blockers) == 1 && p.Blockers[0].Code == "option_exit_order_evidence_unavailable"
}

func TestOptionExitExpiryCloseProposesInTheMoneyCallInTheExpiryWindow(t *testing.T) {
	engine, _, pos, now := expiryCallFixture(t, 7, 105, 1.20, 1.25)
	proposals := generateOptionExits(t, engine, pos, now)
	if len(proposals) != 1 {
		t.Fatalf("want one expiry close, got %+v", proposals)
	}
	p := proposals[0]
	if p.Bucket != rpc.TradeProposalBucketOptionExpiryClose || p.OptionExit == nil || p.OptionExit.Kind != risk.OptionExitActionExpiryClose ||
		p.OrderType != rpc.OrderTypeLMT || p.TIF != rpc.OrderTIFDay || p.Trail != nil || p.LimitPrice != nil ||
		p.Action != rpc.OrderActionSell || p.Quantity != 2 || p.MaxQuantity != 2 || p.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("expiry close is not a full DAY patient limit close: %+v", p)
	}
	if p.Reason != "in-the-money long option, 7 days to expiry; closing avoids exercise at expiry" {
		t.Fatalf("reason = %q", p.Reason)
	}
	if x := p.OptionExit; x.Intent != "directional" || x.DTE != 7 || x.ExpiryCloseDTE != 7 || x.UnderlyingPrice == nil || *x.UnderlyingPrice != 105 ||
		x.ReturnPct == nil || math.Abs(*x.ReturnPct-20) > 1e-9 || x.HighWaterPerShare != nil {
		t.Fatalf("expiry close evidence = %+v", x)
	}
	// The same gates as the loss exit: the all-client working-order check
	// still applies, and the bucket can never place itself.
	if !hermeticOptionExitBlockersOnly(p) || !proposalIsOptionExit(p) || automaticBucketFor(p) != "" {
		t.Fatalf("expiry close bypassed a loss-exit gate: %+v", p.Blockers)
	}
	if counts := proposalCounts(proposals, "USD"); counts.OptionExpiryClose != 1 || counts.OptionLossExit != 0 {
		t.Fatalf("expiry close not counted: %+v", counts)
	}
	// Preview re-evaluates the newer quote against the same moneyness evidence.
	preview := approvedOptionExitPreview(p, now)
	preview.Quote.Bid, preview.Quote.Ask = new(1.20), new(1.25)
	if blockers := proposalPreviewSafetyBlockers(p, preview); len(blockers) != 0 {
		t.Fatalf("expiry close did not survive its own preview: %+v", blockers)
	}
	preview.Quote.Bid, preview.Quote.Ask = new(0.35), new(0.36)
	if !hasTradingBlocker(proposalPreviewSafetyBlockers(p, preview), "option_exit_threshold_changed") {
		t.Fatal("a preview at the loss line kept the expiry close")
	}
}

func TestOptionExitExpiryCloseStaysOutOfTheMoneyAndOutsideTheWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		dte        int
		underlying float64
	}{
		"out of the money at 7 DTE": {7, 95},
		"in the money at 8 DTE":     {8, 105},
	} {
		t.Run(name, func(t *testing.T) {
			engine, _, pos, now := expiryCallFixture(t, tc.dte, tc.underlying, 1.20, 1.25)
			if proposals := generateOptionExits(t, engine, pos, now); len(proposals) != 0 {
				t.Fatalf("no exit applies, got %+v", proposals)
			}
		})
	}
}

func TestOptionExitLossExitWinsOverExpiryClose(t *testing.T) {
	engine, _, pos, now := expiryCallFixture(t, 7, 105, 0.35, 0.36)
	proposals := generateOptionExits(t, engine, pos, now)
	if len(proposals) != 1 || proposals[0].Bucket != rpc.TradeProposalBucketOptionLossExit || proposals[0].OptionExit.Kind != risk.OptionExitActionLoss ||
		!hermeticOptionExitBlockersOnly(proposals[0]) {
		t.Fatalf("want exactly one loss exit for the leg, got %+v", proposals)
	}
}

// Without this refresh's exact underlying price the row cannot tell a close
// from a hold: it is a blocked review with the typed code, not a guess from a
// stale mark or a shared cache, and not a claim of missing option evidence.
func TestOptionExitExpiryWindowWithoutUnderlyingBlocksWithTypedCode(t *testing.T) {
	engine, f, pos, now := expiryCallFixture(t, 7, 105, 1.20, 1.25)
	f.models[42].Underlying = nil // the book evidence fails as a whole
	pos.Options[0].Underlying = new(105.0)
	proposals := generateOptionExits(t, engine, pos, now)
	if len(proposals) != 1 {
		t.Fatalf("want one blocked review, got %+v", proposals)
	}
	p := proposals[0]
	if p.Bucket != rpc.TradeProposalBucketOptionExitReview || p.State != rpc.TradeProposalStateBlocked || p.OptionExit.Kind != "review" ||
		p.OptionExit.Readiness != "blocked" || p.OptionExit.UnderlyingPrice != nil || !hasTradingBlocker(p.Blockers, "option_expiry_underlying_unavailable") ||
		hasTradingBlocker(p.Blockers, "option_exit_measurement_unavailable") {
		t.Fatalf("unavailable underlying did not block with its code: %+v", p)
	}
	for _, b := range p.Blockers {
		if b.Code == "option_expiry_underlying_unavailable" && (b.Message == "" || b.Action == "" || strings.Contains(b.Action, "`")) {
			t.Fatalf("typed blocker lacks a plain next step: %+v", b)
		}
	}
	if !strings.Contains(p.Reason, "underlying price is unavailable") {
		t.Fatalf("reason = %q", p.Reason)
	}
}

// Protection is never sold before expiry: a hedge-listed put the Rulebook
// measures as protection, or one the standing default keeps as protection
// while its role is unmeasured, stays a hedge even when it is in the money. The
// same put measured directional qualifies, as it does for the loss exit.
func TestOptionExitExpiryCloseNeverSellsProtection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  string
		close bool
	}{
		{"measured protection", rpc.OptionHedgeEvidenceMeasured, false},
		{"default protection while unmeasured", rpc.OptionHedgeEvidenceUnmeasured, false},
		{"measured directional", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, pos, now := newOptionEvidenceFixture()
			row := pos.Options[0]
			row.Expiry = now.AddDate(0, 0, 5).Format("20060102")
			pos.Options[0] = row
			contract := *previewIBKRContract(proposalContractFromPosition(row, "OPT"))
			f.scope.Positions[1].Contract = contract
			f.models[row.ConID].Contract = contract
			f.models[row.ConID].Underlying = new(90.0) // put strike 100: in the money
			f.prices = map[int]rpc.OrderQuoteSnapshot{row.ConID: unitQuote(now, 10.20, 10.30)}
			switch tc.role {
			case rpc.OptionHedgeEvidenceMeasured:
				pol := risk.DefaultRulebookPolicy()
				band := 2 * math.Max(pol.RegimeCalm.HedgeBandMaxPct, math.Max(pol.RegimeEarlyWarning.HedgeBandMaxPct, pol.RegimeConfirmed.HedgeBandMaxPct))
				pos.Stocks[0].Quantity = 15000 / (band / 100) / 100
				f.scope.Positions[0].Position = pos.Stocks[0].Quantity
			case rpc.OptionHedgeEvidenceUnmeasured:
				f.readErr = context.DeadlineExceeded
			}
			engine := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
			proposals, _, hedges, _ := engine.generateBook(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
			if tc.close {
				if len(proposals) != 1 || proposals[0].Bucket != rpc.TradeProposalBucketOptionExpiryClose || len(hedges) != 0 {
					t.Fatalf("a measured directional put in the money did not qualify: proposals %+v hedges %+v", proposals, hedges)
				}
				return
			}
			if len(proposals) != 0 || len(hedges) != 1 || hedges[0].Contract.ConID != row.ConID || hedges[0].RoleEvidence != tc.role {
				t.Fatalf("protection was proposed for an expiry close: proposals %+v hedges %+v", proposals, hedges)
			}
		})
	}
}

// The single-option profit trail stays armed from its peak across refreshes,
// measured from the high water its last published row carried; a bid through
// the stop becomes a close now; a changed cost basis starts it again.
func TestOptionExitSingleLegHighWaterCarriesAcrossRefreshes(t *testing.T) {
	engine, f, pos, now := expiryCallFixture(t, 37, 120, 1.60, 1.65)
	publish := func(proposals []rpc.TradeProposal) {
		engine.snapshot = rpc.TradeProposalSnapshot{Proposals: proposals}
	}
	quote := func(bid, ask float64) { f.prices[42] = unitQuote(now, bid, ask) }

	first := generateOptionExits(t, engine, pos, now)
	if len(first) != 1 || first[0].OptionExit.Kind != risk.OptionExitActionProfitTrail || first[0].OptionExit.HighWaterPerShare == nil ||
		*first[0].OptionExit.HighWaterPerShare != 1.60 || first[0].Trail == nil || math.Abs(first[0].Trail.InitialStopPrice-1.12) > 1e-9 {
		t.Fatalf("first refresh did not arm the trail at the 1.60 high water: %+v", first)
	}
	trailKey := first[0].Key
	publish(first)

	// +30% is below the arming line: the carried high water keeps the trail
	// armed, measured from 1.60 rather than from the fresh bid.
	quote(1.30, 1.35)
	second := generateOptionExits(t, engine, pos, now)
	if len(second) != 1 {
		t.Fatalf("the carried trail disarmed: %+v", second)
	}
	trail := second[0]
	if trail.Key != trailKey || trail.OptionExit.Kind != risk.OptionExitActionProfitTrail || trail.OrderType != rpc.OrderTypeTRAILLIMIT || trail.Trail == nil ||
		trail.Trail.TrailingPercent == nil || math.Abs(*trail.Trail.TrailingPercent-30) > 1e-9 || math.Abs(trail.Trail.InitialStopPrice-1.12) > 1e-9 ||
		trail.OptionExit.HighWaterPerShare == nil || *trail.OptionExit.HighWaterPerShare != 1.60 || trail.TrailSizing == nil ||
		trail.TrailSizing.ReferenceSource != "high_water" || !strings.Contains(trail.Reason, "high water of 1.60") || !hermeticOptionExitBlockersOnly(trail) {
		t.Fatalf("second refresh did not trail from the carried high water: %+v", trail)
	}
	preview := approvedOptionExitPreview(trail, now)
	preview.Quote.Bid, preview.Quote.Ask = new(1.30), new(1.35)
	if blockers := proposalPreviewSafetyBlockers(trail, preview); len(blockers) != 0 {
		t.Fatalf("the carried trail did not survive its own preview: %+v", blockers)
	}
	publish(second)

	// The bid is through the carried stop: a trail placed now would trigger
	// on arrival, so the exit is a full DAY patient limit close.
	quote(1.10, 1.15)
	third := generateOptionExits(t, engine, pos, now)
	if len(third) != 1 {
		t.Fatalf("the stop hit produced no close: %+v", third)
	}
	take := third[0]
	if take.Key != trailKey || take.Bucket != rpc.TradeProposalBucketTrailingStop || take.OptionExit.Kind != risk.OptionExitActionProfitTake ||
		take.OrderType != rpc.OrderTypeLMT || take.TIF != rpc.OrderTIFDay || take.Trail != nil || take.LimitPrice != nil || take.Quantity != 2 ||
		take.Reason != "profit trail from the high water of 1.60 was hit" || take.OptionExit.HighWaterPerShare == nil ||
		*take.OptionExit.HighWaterPerShare != 1.60 || !hermeticOptionExitBlockersOnly(take) || automaticBucketFor(take) != "" {
		t.Fatalf("stop hit is not a full close from the high water: %+v", take)
	}
	// The take shares the trail's key but is another order: a review of the
	// trail can never resolve to the take.
	if proposalRevision(rpc.Fingerprint{}, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{}, []rpc.TradeProposal{trail}) ==
		proposalRevision(rpc.Fingerprint{}, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{}, []rpc.TradeProposal{take}) {
		t.Fatal("trail and take share a revision")
	}
	preview = approvedOptionExitPreview(take, now)
	preview.Quote.Bid, preview.Quote.Ask = new(1.10), new(1.15)
	if blockers := proposalPreviewSafetyBlockers(take, preview); len(blockers) != 0 {
		t.Fatalf("the take did not survive its own preview: %+v", blockers)
	}
	publish(third)

	// A changed cost basis resets the high water: +18% against the new cost
	// is below the arming line, and nothing is armed any more.
	pos.Options[0].AvgCost, f.scope.Positions[1].AverageCost = 110, 110
	quote(1.30, 1.35)
	if fourth := generateOptionExits(t, engine, pos, now); len(fourth) != 0 {
		t.Fatalf("a changed cost basis kept the old high water: %+v", fourth)
	}
}

// Units keep their loss exit below the 14-day floor; an armed unit trail below
// it is held back by the floor, named on the row, with the measurement kept.
func TestUnitExitKeepsItsLossExitBelowTheMinimumDTE(t *testing.T) {
	nearExpiry := func(pos *rpc.PositionsResult, now time.Time) {
		for i := range pos.Options {
			pos.Options[i].Expiry = now.AddDate(0, 0, 5).Format("20060102")
		}
	}
	engine, _, pol, pos, now := unitFixture(t, 0.50, 0.55, 0.20, 0.22)
	nearExpiry(pos, now)
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 || proposals[0].OptionExit.Kind != risk.OptionExitActionLoss || proposals[0].State != rpc.TradeProposalStateGenerated ||
		len(proposals[0].Blockers) != 0 || proposals[0].OptionExit.DTE != 5 {
		t.Fatalf("unit loss exit below the floor: %+v", proposals)
	}

	engine, _, pol, pos, now = unitFixture(t, 3.50, 3.55, 0.20, 0.22)
	nearExpiry(pos, now)
	proposals, _ = engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 {
		t.Fatalf("want one held unit row: %+v", proposals)
	}
	held := proposals[0]
	if held.State != rpc.TradeProposalStateBlocked || held.OptionExit.Kind != "review" || !hasTradingBlocker(held.Blockers, "option_exit_min_dte") ||
		hasTradingBlocker(held.Blockers, "option_exit_measurement_unavailable") || held.OptionExit.ReturnPct == nil ||
		held.Unit.HighWaterPerShare != nil || !strings.Contains(held.Reason, "profit trail needs at least 14 DTE") {
		t.Fatalf("armed unit below the floor: %+v", held)
	}

	engine, _, pol, pos, now = unitFixture(t, 2.50, 2.55, 0.20, 0.22)
	nearExpiry(pos, now)
	if proposals, _ = engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now); len(proposals) != 0 {
		t.Fatalf("a mid-range unit below the floor produced a row: %+v", proposals)
	}
}

// A unit's high water resets when its net premium paid changes, as a single
// option's does when its cost basis changes.
func TestUnitExitHighWaterResetsOnChangedCost(t *testing.T) {
	engine, f, pol, pos, now := unitFixture(t, 3.50, 3.55, 0.20, 0.22)
	armed, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(armed) != 1 || armed[0].Unit == nil || armed[0].Unit.HighWaterPerShare == nil {
		t.Fatalf("fixture did not arm the unit: %+v", armed)
	}
	carried := armed[0]
	unit := *carried.Unit
	hwm := 4.0
	unit.HighWaterPerShare = &hwm
	carried.Unit = &unit
	engine.snapshot = rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{carried}}
	// Close 2.50 on 2.00 paid (+25%) sits below the carried stop of 2.80.
	f.prices[42] = unitQuote(now, 2.72, 2.77)
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 || proposals[0].OptionExit.Kind != optionExitUnitProfitTake {
		t.Fatalf("the carried unit high water was not applied: %+v", proposals)
	}
	unit.CostPerShare = 1.50 // recorded against another net premium paid
	engine.snapshot = rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{carried}}
	if proposals, _ = engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now); len(proposals) != 0 {
		t.Fatalf("a changed net premium kept the old high water: %+v", proposals)
	}
}
