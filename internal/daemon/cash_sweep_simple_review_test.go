package daemon

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestCashSweepWholeOrderPreviewNeverResizesForFees(t *testing.T) {
	rig := newSweepPreviewRig(t, time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC))
	original := rig.srv.orderPreviewWhatIf
	rig.srv.orderPreviewWhatIf = func(ctx context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		result, err := original(ctx, draft)
		result.Margin.MaxCommission = new(1000.0)
		return result, err
	}
	out := rig.preview(t)
	if out.Accepted || !hasTradingBlocker(out.Blockers, "cash_sweep_cost_above_free_cash") {
		t.Fatalf("unsafe fee envelope admitted: accepted=%v blockers=%v", out.Accepted, out.Blockers)
	}
	if len(rig.drafts) != 1 || rig.drafts[0].Quantity != rig.row.Quantity || out.Preview == nil || out.Preview.Draft.Quantity != rig.row.Quantity {
		t.Fatalf("reviewed a replacement quantity: calls=%d preview=%+v", len(rig.drafts), out.Preview)
	}
	if out.Preview.CashSweepEconomics == nil || out.Preview.CashSweepEconomics.FeeReserve == nil || *out.Preview.CashSweepEconomics.FeeReserve != 1000 {
		t.Fatal("blocked preview lost its actual fee envelope")
	}
}

func TestCashSweepBenefitIsAdvisoryThroughActualProposalPreview(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		change      func(*rpc.TradeProposalCashSweep)
	}{
		{"below configured benchmark", "below_benchmark", func(s *rpc.TradeProposalCashSweep) { s.MinNetGainBase = 1e9 }},
		{"unknown cash interest", "unavailable", func(s *rpc.TradeProposalCashSweep) { s.CashInterest = nil }},
		{"nonfinite cash interest", "unavailable", func(s *rpc.TradeProposalCashSweep) { s.CashInterest = cashInterestTestSide(math.NaN()) }},
		{"stale cash interest", "unavailable", func(s *rpc.TradeProposalCashSweep) {
			s.CashInterest = &rpc.CashInterestSide{State: rpc.CashInterestStateUnavailable, LastValueDate: "2026-09-24",
				Reason: "the latest USD credit row is dated 2026-09-24, 4 business days ago, so the live rate is unknown"}
		}},
		{"valid zero benchmark", "estimated", func(s *rpc.TradeProposalCashSweep) { s.MinNetGainBase = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSweepPreviewRig(t, time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC))
			s := rig.row.CashSweep
			s.MinNetGainBase = 25
			s.CashInterest = cashInterestTestSide(0.0)
			tc.change(s)
			out := rig.preview(t)
			if !out.Accepted || !out.SubmitEligible || len(out.Blockers) != 0 || out.Preview == nil {
				t.Fatalf("advisory economics blocked normal review: accepted=%v blockers=%v", out.Accepted, out.Blockers)
			}
			if len(rig.drafts) != 1 || rig.drafts[0].Quantity != rig.row.Quantity || out.Preview.Draft.Quantity != rig.row.Quantity {
				t.Fatalf("changed whole order: calls=%d", len(rig.drafts))
			}
			advice := out.Preview.CashSweepEconomics
			if advice == nil || advice.State != tc.state || !advice.AsOf.Equal(rig.now) {
				t.Fatalf("advisory not projected correctly: %+v", advice)
			}
			if tc.state == "unavailable" && advice.IncrementalGainBase != nil {
				t.Fatal("missing comparison was fabricated as zero benefit")
			}
		})
	}
}

func TestCashSweepWholeSaleReportsPartialRestorationWithoutResizing(t *testing.T) {
	rig := newSweepPreviewRig(t, time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC))
	rig.row.Action, rig.row.PositionEffect = rpc.OrderActionSell, rpc.OrderPositionEffectReduce
	rig.row.Quantity, rig.row.PositionQuantity = 50, 100
	rig.row.CashSweep.Side = rpc.CashSweepSideRedeem
	rig.row.CashSweep.QuantityUnit = rpc.CashSweepQuantityPosition
	rig.row.CashSweep.RedemptionTarget = 50000
	rig.srv.orderPreviewPositionImpact = fixedPreviewPosition(100, 50, rpc.OrderPositionEffectReduce)
	out := rig.preview(t)
	if !out.Accepted || !out.SubmitEligible || out.Preview == nil || len(out.Blockers) != 0 {
		t.Fatalf("partial-restoration sale refused: accepted=%v blockers=%v", out.Accepted, out.Blockers)
	}
	if len(rig.drafts) != 1 || rig.drafts[0].Quantity != 50 || out.Preview.Draft.Quantity != 50 {
		t.Fatalf("sale quantity was altered to fit the net target: calls=%d", len(rig.drafts))
	}
	advice := out.Preview.CashSweepEconomics
	if advice == nil || advice.State != "partial_restoration" || advice.NetProceeds == nil || math.Abs(*advice.NetProceeds-49799) > 1e-6 || advice.RemainingGap == nil || math.Abs(*advice.RemainingGap-201) > 1e-6 {
		t.Fatalf("sale cash deficit concealed: %+v", advice)
	}
}

func TestCashSweepAdvisoryNeverMutatesOrRenewsPreview(t *testing.T) {
	prop, preview := sweepFeePreview(30000, 990, 1)
	prop.CashSweep.CashInterest = cashInterestTestSide(0.0)
	prop.CashSweep.Bill.Maturity = "2026-12-31"
	preview.Quote.Ask = new(99.0)
	first := sanitizeProposalPreviewForProposal(preview, prop)
	if first.CashSweepEconomics == nil || first.CashSweepEconomics.FeeReserve == nil || first.CashSweepEconomics.IncrementalGainBase == nil {
		t.Fatal("advisory not available")
	}
	*first.CashSweepEconomics.FeeReserve = 1234
	*first.CashSweepEconomics.IncrementalGainBase = 1234
	second := sanitizeProposalPreviewForProposal(preview, prop)
	if *second.CashSweepEconomics.FeeReserve != 1 || *second.CashSweepEconomics.IncrementalGainBase == 1234 || !second.CashSweepEconomics.AsOf.Equal(preview.AsOf) || second.Draft.Quantity != preview.Draft.Quantity {
		t.Fatal("a consumer changed or renewed the reviewed evidence")
	}
}

// A negative estimate is no longer advisory: the buy is blocked (owner
// decision 2026-10-23 13:16 CEST), and the preview still shows the estimate.
func TestCashSweepNegativeBenefitBlocksThroughActualProposalPreview(t *testing.T) {
	rig := newSweepPreviewRig(t, time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC))
	s := rig.row.CashSweep
	s.MinNetGainBase, s.CashInterest = 0, cashInterestTestSide(1.0)
	out := rig.preview(t)
	if out.Accepted || !hasTradingBlocker(out.Blockers, rpc.CashSweepBlockerNoNetGain) {
		t.Fatalf("a sweep worse than cash passed review: accepted=%v blockers=%v", out.Accepted, out.Blockers)
	}
	if out.Preview == nil || out.Preview.CashSweepEconomics == nil || out.Preview.CashSweepEconomics.State != "estimated" {
		t.Fatal("the blocked preview lost its estimate")
	}
}
