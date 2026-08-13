package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func optionExitTestTime() time.Time {
	// Wednesday 10:00 America/New_York during the listed-options session.
	return time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
}

func optionExitTestRow() rpc.PositionView {
	now := optionExitTestTime()
	bid, ask := 1.50, 1.55
	return rpc.PositionView{
		Symbol: "TEST", SecType: "OPTION", ConID: 42, Exchange: "SMART", Currency: "USD",
		LocalSymbol: "TEST  260918C00100000", TradingClass: "TEST", Quantity: 2,
		Multiplier: 100, AvgCost: 100, Mark: 1.52, MarketValue: 304,
		DataType: rpc.MarketDataLive, PriceAt: now, Expiry: "20260918", Strike: 100, Right: "C",
		OptionBid: &bid, OptionAsk: &ask,
	}
}

func enabledOptionExitPolicy() protectionPolicy {
	pol := defaultProtectionPolicy()
	pol.PolicyVersion = 5
	pol.Buckets.TrailingStop.Options.Enabled = true
	pol.Buckets.TrailingStop.Options.limitOffsetExplicit = true
	pol.Buckets.TrailingStop.Options.DirectionalIntents = []protectionOptionDirectionalIntent{{
		ConID: 42, Reason: "standalone directional option",
		ApprovedAt: optionExitTestTime().Add(-time.Hour), ExpiresAt: optionExitTestTime().Add(24 * time.Hour),
	}}
	return pol
}

func TestOptionExitProposalApprovedProfitTrail(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.05, lossExitPct)
	if !ok {
		t.Fatal("expected profit-trail proposal")
	}
	if proposal.Bucket != rpc.TradeProposalBucketTrailingStop || proposal.OrderType != rpc.OrderTypeTRAILLIMIT || proposal.TIF != rpc.OrderTIFDay {
		t.Fatalf("proposal semantics = bucket %q type %q tif %q", proposal.Bucket, proposal.OrderType, proposal.TIF)
	}
	if proposal.Action != rpc.OrderActionSell || proposal.Quantity != 2 || proposal.MaxQuantity != 2 || proposal.Trail == nil || proposal.Trail.TrailingPercent == nil || proposal.Trail.TrailingAmount != nil {
		t.Fatalf("proposal sizing = %+v", proposal)
	}
	if math.Abs(*proposal.Trail.TrailingPercent-30) > 1e-9 {
		t.Fatalf("native broker trail percent = %.8f, want 30", *proposal.Trail.TrailingPercent)
	}
	if proposal.OptionExit == nil || proposal.OptionExit.Kind != risk.OptionExitActionProfitTrail || proposal.OptionExit.InitialLockedGainPct == nil || *proposal.OptionExit.InitialLockedGainPct < pol.Buckets.TrailingStop.Options.LockedGainPct {
		t.Fatalf("option exit context = %+v", proposal.OptionExit)
	}
	if len(proposal.Blockers) != 0 {
		t.Fatalf("unexpected blockers = %+v", proposal.Blockers)
	}
}

func TestOptionExitFloorAdjustedNativeTrailSurvivesPreviewAndWireMapping(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	*row.OptionBid, *row.OptionAsk = 2.00, 2.35
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.05, lossExitPct)
	if !ok || proposal.Trail == nil || proposal.Trail.TrailingPercent == nil {
		t.Fatalf("expected floor-adjusted native option trail: %+v", proposal)
	}
	if proposal.Trail.TrailingAmount != nil || math.Abs(*proposal.Trail.TrailingPercent-35) > 1e-9 ||
		math.Abs(proposal.Trail.InitialStopPrice-1.30) > 1e-9 || proposal.Trail.LimitOffset == nil ||
		math.Abs(*proposal.Trail.LimitOffset-0.05) > 1e-9 {
		t.Fatalf("native floor-adjusted trail = %+v, want 35%% / stop 1.30 / offset 0.05 with no amount", proposal.Trail)
	}
	if proposal.OptionExit == nil || proposal.OptionExit.InitialLockedGainPct == nil ||
		*proposal.OptionExit.InitialLockedGainPct < pol.Buckets.TrailingStop.Options.LockedGainPct {
		t.Fatalf("floor-adjusted trail weakened locked gain: %+v", proposal.OptionExit)
	}

	preview := approvedOptionExitPreview(proposal, now)
	preview.Quote.Bid, preview.Quote.Ask = row.OptionBid, row.OptionAsk
	if blockers := proposalPreviewSafetyBlockers(proposal, preview); len(blockers) != 0 {
		t.Fatalf("approved native trail did not survive preview: %+v", blockers)
	}
	wire := previewIBKROrder(preview.Draft)
	if wire.OrderType != rpc.OrderTypeTRAILLIMIT || wire.AuxPrice != 0 || wire.LmtPrice != 0 || wire.LmtPriceSet ||
		math.Abs(wire.TrailingPercent-35) > 1e-9 || math.Abs(wire.TrailStopPrice-1.30) > 1e-9 ||
		math.Abs(wire.LmtPriceOffset-0.05) > 1e-9 {
		t.Fatalf("native trail wire mapping = %+v", wire)
	}
}

func TestOptionExitPreviewBlocksNativeTrailFieldDrift(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	*row.OptionBid, *row.OptionAsk = 2.00, 2.35
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.05, lossExitPct)
	if !ok {
		t.Fatal("expected floor-adjusted option trail")
	}

	for _, tc := range []struct {
		name  string
		code  string
		drift func(*rpc.OrderPreviewResult)
	}{
		{name: "percent", code: "trail_percent_drift", drift: func(p *rpc.OrderPreviewResult) { *p.Draft.Trail.TrailingPercent += 1 }},
		{name: "initial stop", code: "trail_initial_stop_drift", drift: func(p *rpc.OrderPreviewResult) { p.Draft.Trail.InitialStopPrice += 0.05 }},
		{name: "limit offset", code: "trail_limit_offset_drift", drift: func(p *rpc.OrderPreviewResult) { *p.Draft.Trail.LimitOffset += 0.05 }},
		{name: "amount replaces percent", code: "trail_offset_type_drift", drift: func(p *rpc.OrderPreviewResult) {
			p.Draft.Trail.OffsetType = rpc.OrderTrailOffsetAmount
			p.Draft.Trail.TrailingAmount = cloneFloat64Ptr(&decision.TrailAmount)
			p.Draft.Trail.TrailingPercent = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview := approvedOptionExitPreview(proposal, now)
			preview.Quote.Bid, preview.Quote.Ask = row.OptionBid, row.OptionAsk
			tc.drift(preview)
			blockers := proposalPreviewSafetyBlockers(proposal, preview)
			if !hasTradingBlocker(blockers, tc.code) {
				t.Fatalf("missing %s blocker: %+v", tc.code, blockers)
			}
		})
	}
}

func TestOptionExitProposalApprovedLossIsPatientLimit(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	*row.OptionBid, *row.OptionAsk = 0.40, 0.42
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.01, lossExitPct)
	if !ok {
		t.Fatal("expected loss-exit proposal")
	}
	if proposal.Bucket != rpc.TradeProposalBucketOptionLossExit || proposal.OrderType != rpc.OrderTypeLMT || proposal.TIF != rpc.OrderTIFDay || proposal.Trail != nil || proposal.LimitPrice != nil {
		t.Fatalf("loss proposal semantics = %+v", proposal)
	}
	if proposal.Action != rpc.OrderActionSell || proposal.Quantity != 2 || proposal.MaxQuantity != 2 {
		t.Fatalf("loss proposal sizing = %+v", proposal)
	}
}

func TestValidateOptionExitPolicyRequiresExactSortedIntentAndDAY(t *testing.T) {
	pol := enabledOptionExitPolicy()
	if err := validateProtectionPolicy(pol); err != nil {
		t.Fatalf("approved policy: %v", err)
	}

	pol.Buckets.TrailingStop.Options.DirectionalIntents = []protectionOptionDirectionalIntent{
		{ConID: 42, Reason: "first", ApprovedAt: optionExitTestTime().Add(-time.Hour), ExpiresAt: optionExitTestTime().Add(time.Hour)},
		{ConID: 41, Reason: "second", ApprovedAt: optionExitTestTime().Add(-time.Hour), ExpiresAt: optionExitTestTime().Add(time.Hour)},
	}
	if err := validateProtectionPolicy(pol); err == nil {
		t.Fatal("expected unsorted exact-contract intent to fail")
	}

	pol = enabledOptionExitPolicy()
	pol.Buckets.TrailingStop.Options.TIF = rpc.OrderTIFGTC
	if err := validateProtectionPolicy(pol); err == nil {
		t.Fatal("expected option GTC trail to fail")
	}

	pol = enabledOptionExitPolicy()
	pol.Buckets.TrailingStop.Options.OrderType = rpc.OrderTypeTRAIL
	if err := validateProtectionPolicy(pol); err == nil {
		t.Fatal("expected option TRAIL market trigger to fail")
	}
}

func TestValidateOptionExitPolicyRefusesImplicitLimitOffsetOnActivation(t *testing.T) {
	pol := enabledOptionExitPolicy()
	pol.Buckets.TrailingStop.Options.limitOffsetExplicit = false
	if err := validateProtectionPolicy(pol); err == nil {
		t.Fatal("enabled option exits inherited an unapproved dormant limit offset")
	}
}

func TestValidateOptionExitPolicyRejectsTOMLNonFiniteValues(t *testing.T) {
	for _, raw := range []string{"value = nan", "value = +inf", "value = -inf"} {
		var fixture struct {
			Value float64 `toml:"value"`
		}
		if _, err := toml.Decode(raw, &fixture); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if !math.IsNaN(fixture.Value) && !math.IsInf(fixture.Value, 0) {
			t.Fatalf("fixture %q did not produce non-finite value", raw)
		}
		pol := enabledOptionExitPolicy()
		pol.Buckets.TrailingStop.Options.ProfitArmGainPct = fixture.Value
		if err := validateProtectionPolicy(pol); err == nil {
			t.Fatalf("policy accepted %q", raw)
		}
	}
}

func TestOptionExitPreviewRequiresFullExactContractQuantity(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.05, lossExitPct)
	if !ok {
		t.Fatal("expected option-exit proposal")
	}
	preview := &rpc.OrderPreviewResult{
		Draft: rpc.OrderDraft{
			Action: proposal.Action, Contract: proposal.Contract, Quantity: 1,
			OrderType: proposal.OrderType, Trail: cloneTrailSpec(proposal.Trail), TIF: proposal.TIF,
			TriggerMethod: proposalTriggerMethod(proposal), Source: proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	blockers := proposalPreviewSafetyBlockers(proposal, preview)
	for _, blocker := range blockers {
		if blocker.Code == "option_exit_full_quantity_required" {
			return
		}
	}
	t.Fatalf("missing full-quantity blocker: %+v", blockers)
}

func TestOptionExitPreviewBlocksFreshPositionGrowth(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.05, lossExitPct)
	if !ok {
		t.Fatal("expected option-exit proposal")
	}
	preview := approvedOptionExitPreview(proposal, now)
	preview.Position.Before = 3
	preview.Position.After = 1
	if !hasTradingBlocker(proposalPreviewSafetyBlockers(proposal, preview), "option_exit_fresh_full_close_required") {
		t.Fatalf("missing fresh full-close blocker: %+v", proposalPreviewSafetyBlockers(proposal, preview))
	}
}

func TestOptionExitPreviewReevaluatesFreshThreshold(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.05, lossExitPct)
	if !ok {
		t.Fatal("expected option-exit proposal")
	}
	preview := approvedOptionExitPreview(proposal, now)
	bid, ask := 1.20, 1.25
	preview.Quote.Bid, preview.Quote.Ask = &bid, &ask
	if !hasTradingBlocker(proposalPreviewSafetyBlockers(proposal, preview), "option_exit_threshold_changed") {
		t.Fatalf("missing fresh threshold blocker: %+v", proposalPreviewSafetyBlockers(proposal, preview))
	}
}

func TestOptionExitMissingQuoteEmitsBlockedReviewProposal(t *testing.T) {
	pol := enabledOptionExitPolicy()
	row := optionExitTestRow()
	row.OptionBid, row.OptionAsk = nil, nil
	row.PriceAt = time.Time{}
	row.DataType = ""
	now := optionExitTestTime()
	lossExitPct := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, lossExitPct)
	proposal, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0, lossExitPct)
	if !ok || proposal.Bucket != rpc.TradeProposalBucketOptionExitReview || proposal.State != rpc.TradeProposalStateBlocked {
		t.Fatalf("review proposal = %+v, ok=%t", proposal, ok)
	}
	if !hasTradingBlocker(proposal.Blockers, "option_exit_measurement_unavailable") {
		t.Fatalf("missing measurement blocker: %+v", proposal.Blockers)
	}
}

func TestProposalKeySeparatesExactOptionContracts(t *testing.T) {
	base := rpc.ContractParams{Symbol: "SPX", SecType: "OPT", Exchange: "SMART", TradingClass: "SPXW", Expiry: "20260918", Right: "P", Strike: 5000}
	first, second := base, base
	first.ConID, second.ConID = 1001, 1002
	if proposalKey(rpc.TradeProposalBucketTrailingStop, first, rpc.OrderActionSell) == proposalKey(rpc.TradeProposalBucketTrailingStop, second, rpc.OrderActionSell) {
		t.Fatal("exact option contracts collided")
	}
}

func TestOptionExitHedgeListedPutFailsClosedOnIncompleteRoleEvidence(t *testing.T) {
	row := optionExitTestRow()
	row.Symbol, row.Right = "SPY", "P"
	allowed, role := optionExitEconomicRole(row, risk.DefaultRulebookPolicy())
	if allowed || role != risk.IndexPutRoleUnclassified {
		t.Fatalf("allowed=%t role=%q, want blocked unclassified", allowed, role)
	}
}

func TestOptionExitConIDLessBrokerOrderMatchesConservatively(t *testing.T) {
	contract := rpc.ContractParams{ConID: 42, Symbol: "SPX", SecType: "OPT", Currency: "USD", Expiry: "20260918", Right: "P", Strike: 5000, LocalSymbol: "SPXW  260918P05000000", TradingClass: "SPXW"}
	for _, order := range []ibkrlib.OrderLifecycleEvent{
		{Symbol: "SPX", SecType: "OPT", Currency: "USD", Expiry: "20260918", Right: "P", Strike: 5000, TradingClass: "SPXW"},
		{Symbol: "SPX", SecType: "OPT", Currency: "USD", Expiry: "20260918", Right: "P", Strike: 5000, LocalSymbol: contract.LocalSymbol},
		{Symbol: "SPX", SecType: "OPT"},
	} {
		if !optionExitSnapshotContractCouldMatch(order, contract) {
			t.Fatalf("plausible ConID-less order did not match: %+v", order)
		}
	}
	other := ibkrlib.OrderLifecycleEvent{Symbol: "SPX", SecType: "OPT", Expiry: "20260918", Right: "P", Strike: 5100}
	if optionExitSnapshotContractCouldMatch(other, contract) {
		t.Fatal("contradictory strike matched exact contract")
	}
}

func approvedOptionExitPreview(proposal rpc.TradeProposal, now time.Time) *rpc.OrderPreviewResult {
	row := optionExitTestRow()
	return &rpc.OrderPreviewResult{
		Draft: rpc.OrderDraft{
			Action: proposal.Action, Contract: proposal.Contract, Quantity: proposal.Quantity,
			OrderType: proposal.OrderType, Trail: cloneTrailSpec(proposal.Trail), TIF: proposal.TIF,
			TriggerMethod: proposalTriggerMethod(proposal), Source: proposalOrderSource,
		},
		Quote: rpc.OrderQuoteSnapshot{
			Bid: row.OptionBid, Ask: row.OptionAsk, DataType: rpc.MarketDataLive, PriceAt: now, AsOf: now,
			SessionContext: &rpc.MarketSession{IsOpen: true},
		},
		Position: rpc.OrderPositionImpact{Before: row.Quantity, After: 0, Effect: rpc.OrderPositionEffectClose, AverageCost: row.AvgCost, Multiplier: row.Multiplier},
	}
}

func hasTradingBlocker(blockers []rpc.TradingBlocker, code string) bool {
	for _, blocker := range blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}
