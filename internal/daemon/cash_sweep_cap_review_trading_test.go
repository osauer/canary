//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The synthetic broker and real journal/token rig exercise all production
// submission paths without connecting to IBKR. The global trading limit is
// deliberately larger than the sweep bucket's independent limit.
func sweepRedemptionCapRig(t *testing.T, cap, limit float64, authority string) (*automaticTestRig, rpc.TradeProposal, *brokerCallLog) {
	t.Helper()
	rig := newAutomaticTradingRig(t, authority)
	setTestOrderLimits(rig.server, func(o *risk.ConstitutionOrderLimits) { o.MaxOrderFloorBase = new(1e6) })
	broker := &brokerCallLog{}
	broker.install(rig.server)
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, cap)
	setSweepCcy(policy.Cash.Sweep, "EUR", func(c *protectionCashSweepCurrency) { c.MinTranche = new(900.0) })
	in := eurRedeemInput(10000)
	maturity := cashSweepDay(rig.now).AddDate(0, 0, 60)
	in.Holdings["EUR"][0].Maturity = maturity
	line := synthBondLine(7401, synthDEBill2, "EUR", maturity)
	src := &fakeBillSource{heldLines: map[int][]ibkrlib.BondContractDetails{7401: {line}}}
	plan := cashSweepPlanFor(policy, in, rig.now)
	cashSweepResolveBills(t.Context(), src, policy.Cash.Sweep, &plan, rig.now)
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, rig.now, plan, cashSweepCurrencyOf(t, plan, "EUR"))
	if row.Quantity != 1000 || row.MaxQuantity != 1000 || len(row.Blockers) != 0 {
		t.Fatalf("fixture redemption = %d/%d blockers %v", row.Quantity, row.MaxQuantity, row.Blockers)
	}
	rig.server.orderBondDetailsForTest = func(context.Context, int, string) ([]ibkrlib.BondContractDetails, error) {
		return []ibkrlib.BondContractDetails{line}, nil
	}
	rig.server.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := limit-0.02, limit+0.02
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	rig.server.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, _ string, qty int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{Before: 10000, After: 10000 - float64(qty), Effect: rpc.OrderPositionEffectReduce}, nil
	}
	rig.server.orderFXRateForTest = func(context.Context, string, string, time.Duration) (float64, time.Time, error) {
		return 1, rig.now, nil
	}
	rig.server.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true, Margin: &rpc.OrderMarginImpact{CommissionCurrency: "EUR", MaxCommission: new(1.0)}}, nil
	}
	row.Revision = rig.install(row)
	return rig, row, broker
}

func TestCashSweepRedemptionRequestedQuantityCannotWidenTheCappedTranche(t *testing.T) {
	for _, path := range []string{"preview", "prepare", "submit", "prepared-submit"} {
		t.Run(path, func(t *testing.T) {
			rig, row, broker := sweepRedemptionCapRig(t, 1500, 99.5, "")
			params := rpc.TradeProposalPreviewParams{Key: row.Key, Revision: row.Revision, Quantity: 10000, FastPath: true}
			var preview *rpc.TradeProposalOrderPreview
			switch path {
			case "preview":
				out, err := rig.engine.Preview(t.Context(), params)
				if err != nil || !out.Accepted {
					t.Fatalf("preview: accepted %v blockers %v err %v", out.Accepted, out.Blockers, err)
				}
				preview = out.Preview
			case "prepare", "prepared-submit":
				out, err := rig.engine.Prepare(t.Context(), params)
				if err != nil || !out.Accepted {
					t.Fatalf("prepare: accepted %v blockers %v err %v", out.Accepted, out.Blockers, err)
				}
				preview = out.Preview
				if path == "prepared-submit" {
					result := preparedSubmit(t, rig, out)
					if !result.Accepted || broker.count() != 1 {
						t.Fatalf("prepared send: accepted %v blockers %v broker calls %d", result.Accepted, result.Blockers, broker.count())
					}
				}
			case "submit":
				out, err := rig.engine.Submit(t.Context(), rpc.TradeProposalSubmitParams{Key: row.Key, Revision: row.Revision, Quantity: 10000, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
				if err != nil || !out.Accepted || broker.count() != 1 {
					t.Fatalf("direct send: accepted %v blockers %v broker calls %d err %v", out.Accepted, out.Blockers, broker.count(), err)
				}
				preview = out.Preview
			}
			if preview == nil || preview.Draft.Quantity != 1000 || preview.Notional > 1500 {
				t.Fatalf("requested quantity bypassed sweep cap: %+v", preview)
			}
			broker.mu.Lock()
			defer broker.mu.Unlock()
			for _, order := range broker.orders {
				if order.TotalQty != 1000 || float64(order.TotalQty)*order.LmtPrice/100 > 1500 {
					t.Fatalf("broker received an order above the tranche: %+v", order)
				}
			}
		})
	}
}

func TestCashSweepRedemptionFreshPriceCannotExceedTheBucketCap(t *testing.T) {
	for _, path := range []string{"preview", "prepare", "submit", "prepared-submit", "automatic"} {
		t.Run(path, func(t *testing.T) {
			authority := ""
			if path == "automatic" {
				authority = `pre_authorised = ["cash_sweep"]`
			}
			// Planning at 99.5 permits 1,000 units; the fresh 99.54 limit
			// costs 995.40, above the independently configured 995.01 cap.
			rig, row, broker := sweepRedemptionCapRig(t, 995.01, 99.54, authority)
			params := rpc.TradeProposalPreviewParams{Key: row.Key, Revision: row.Revision, FastPath: true}
			var blockers []rpc.TradingBlocker
			switch path {
			case "preview":
				out, err := rig.engine.Preview(t.Context(), params)
				if err != nil || out.Accepted {
					t.Fatalf("preview above cap: accepted %v err %v", out.Accepted, err)
				}
				blockers = out.Blockers
			case "prepare":
				out, err := rig.engine.Prepare(t.Context(), params)
				if err != nil || out.Accepted || out.PreparedRef != "" {
					t.Fatalf("prepared above cap: accepted %v ref present %v err %v", out.Accepted, out.PreparedRef != "", err)
				}
				blockers = out.Blockers
			case "submit":
				out, err := rig.engine.Submit(t.Context(), rpc.TradeProposalSubmitParams{Key: row.Key, Revision: row.Revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
				if err != nil || out.Accepted {
					t.Fatalf("direct send above cap: accepted %v err %v", out.Accepted, err)
				}
				blockers = out.Blockers
			case "prepared-submit":
				// Model a preparation retained by the earlier binary, whose
				// generic preview admitted this over-cap redemption. The new
				// prepared-submit safety check must refuse it too.
				preview, err := rig.server.previewOrder(t.Context(), proposalOrderPreviewParams(row, row.Quantity, 0))
				if err != nil || !preview.SubmitEligible {
					t.Fatalf("legacy preview: err %v", err)
				}
				ref, preparation, err := rig.engine.retainPreparation(t.Context(), row, preview)
				if err != nil {
					t.Fatal(err)
				}
				retained := rpc.TradeProposalPrepareResult{PreparedRef: ref, Preparation: preparation,
					TradeProposalPreviewResult: rpc.TradeProposalPreviewResult{Proposal: row, Preview: sanitizeProposalPreviewForProposal(preview, row)}}
				out := preparedSubmit(t, rig, retained)
				if out.Accepted {
					t.Fatal("legacy prepared redemption above cap was submitted")
				}
				blockers = out.Blockers
			case "automatic":
				rig.engine.reconcileAutomatic(t.Context())
				rec := rig.record(row.Key, row.Revision)
				rig.notice(rec)
				rig.now = rec.SubmitAt.Add(time.Minute)
				rig.engine.submitAutomatic(t.Context(), rec)
				finished := rig.record(row.Key, row.Revision)
				if finished.State != rpc.TradeProposalAutomaticFailed || !strings.Contains(finished.Reason, "above max_order_notional") {
					t.Fatalf("automatic over-cap verdict = %s (%s)", finished.State, finished.Reason)
				}
				blockers = []rpc.TradingBlocker{{Code: "cash_sweep_above_max_order_notional"}}
			}
			if !hasTradingBlocker(blockers, "cash_sweep_above_max_order_notional") || broker.count() != 0 {
				t.Fatalf("over-cap redemption reached broker: blockers %v calls %d", blockers, broker.count())
			}
		})
	}
}
