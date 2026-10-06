//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// levelingBundleRig serves the two-conversion repayment of a synthetic USD
// loan (−14,000) from CHF (+6,000, 0%) and EUR (+60,000, 1.4%) on the
// trading rig, with live IDEALPRO quotes for both pairs; each conversion
// stays under the rig's own order cap in force (10,000 USD). The rig's clock
// is a Thursday morning, inside IDEALPRO's hours.
func levelingBundleRig(t *testing.T) (*automaticTestRig, []rpc.TradeProposal, rpc.TradeProposalCurrencyLevelingBundle, *brokerCallLog, *atomic.Int32) {
	t.Helper()
	rig := newAutomaticTradingRig(t, "")
	broker := &brokerCallLog{}
	broker.install(rig.server)
	whatIfs := &atomic.Int32{}
	original := rig.server.orderPreviewWhatIf
	rig.server.orderPreviewWhatIf = func(ctx context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		whatIfs.Add(1)
		return original(ctx, draft)
	}
	rig.server.orderContractResolverForTest = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.ContractParams, error) {
		c.MinTick = 0.00005
		return c, nil
	}
	rig.server.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 1.16995, 1.17005 // EUR.USD
		if c.Symbol == "USD" {
			bid, ask = 0.79870, 0.79880 // USD.CHF
		}
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	rig.server.orderPreviewPositionImpact = fixedPreviewPosition(0, 0, rpc.OrderPositionEffectOpenShort)
	in := levelingBook(map[string]float64{"EUR": 60000, "CHF": 6000, "USD": -14000})
	rows := levelingRows(t, in, "USD")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the CHF and EUR conversions", len(rows))
	}
	var bundle rpc.TradeProposalCurrencyLevelingBundle
	rig.installWith(func(snap *rpc.TradeProposalSnapshot) {
		bundle = rpc.TradeProposalCurrencyLevelingBundle{ID: rows[0].CurrencyLeveling.BundleID, Currency: "USD", PaybackDays: 30}
		for _, row := range snap.Proposals {
			bundle.Keys = append(bundle.Keys, row.Key)
		}
		snap.CurrencyLeveling = &rpc.TradeProposalCurrencyLevelingStatus{BaseCurrency: "EUR", Bundles: []rpc.TradeProposalCurrencyLevelingBundle{bundle}}
		currencyLevelingBundleRevisions(snap.CurrencyLeveling, snap.Proposals)
		bundle = snap.CurrencyLeveling.Bundles[0]
	}, rows...)
	served := rig.engine.Snapshot(false).Proposals
	return rig, served, bundle, broker, whatIfs
}

func submitBundle(t *testing.T, rig *automaticTestRig, ref string, bundle rpc.TradeProposalCurrencyLevelingBundle) *rpc.TradeProposalSubmitBundleResult {
	t.Helper()
	raw, err := json.Marshal(rpc.TradeProposalSubmitBundleParams{BundleRef: ref, BundleID: bundle.ID, Revision: bundle.Revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil {
		t.Fatal(err)
	}
	out, err := rig.server.handleTradeProposalsSubmitBundle(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A conversion that is one of several is never prepared or sent on its own;
// the bundle is prepared whole, then sent whole, cheapest payer first, and
// its reference is spent.
func TestCurrencyLevelingBundleOneApproval(t *testing.T) {
	rig, rows, bundle, broker, whatIfs := levelingBundleRig(t)
	for _, row := range rows {
		single, err := rig.engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: row.Key, Revision: row.Revision})
		if err != nil || single.Accepted || single.PreparedRef != "" || !hasBlockerCode(single.Blockers, rpc.CurrencyLevelingBlockerBundle) {
			t.Fatalf("single prepare of %s = %+v, %v; want the bundle refusal", row.Symbol, single.Blockers, err)
		}
		fast, err := rig.engine.Submit(t.Context(), rpc.TradeProposalSubmitParams{Key: row.Key, Revision: row.Revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
		if err != nil || fast.Accepted || !hasBlockerCode(fast.Blockers, rpc.CurrencyLevelingBlockerBundle) {
			t.Fatalf("single submit of %s = %+v, %v; want the bundle refusal", row.Symbol, fast.Blockers, err)
		}
	}
	if whatIfs.Load() != 0 || broker.count() != 0 {
		t.Fatalf("a refused single approval reached the broker: %d previews, %d orders", whatIfs.Load(), broker.count())
	}
	prepared, err := rig.engine.PrepareBundle(t.Context(), rpc.TradeProposalPrepareBundleParams{BundleID: bundle.ID, Revision: bundle.Revision})
	if err != nil || !prepared.Accepted || prepared.BundleRef == "" || len(prepared.Legs) != 2 || whatIfs.Load() != 2 {
		t.Fatalf("prepare bundle = %+v, %v (previews %d)", prepared, err, whatIfs.Load())
	}
	for _, leg := range prepared.Legs {
		if !leg.Accepted || !leg.SubmitEligible {
			t.Fatalf("leg %s not prepared: %+v", leg.Proposal.Symbol, leg.Blockers)
		}
	}
	if raw, _ := json.Marshal(prepared.Legs); strings.Contains(string(raw), preparedProposalPrefix+".") {
		t.Fatal("a conversion's private reference left the daemon")
	}
	out := submitBundle(t, rig, prepared.BundleRef, bundle)
	if !out.Accepted || len(out.Legs) != 2 || broker.count() != 2 {
		t.Fatalf("submit bundle = %+v (orders %d)", out, broker.count())
	}
	if first := broker.orders[0]; first.Action != rpc.OrderActionBuy {
		t.Fatalf("first order = %+v, want the CHF conversion (BUY USD.CHF) first", first)
	}
	// The reference is spent: a second send finds the first conversion
	// consumed and sends nothing.
	again := submitBundle(t, rig, prepared.BundleRef, bundle)
	if again.Accepted || broker.count() != 2 || !hasBlockerCode(again.Blockers, "bundle_conversion_refused") {
		t.Fatalf("second submit = %+v (orders %d)", again, broker.count())
	}
}

// A bundle whose second conversion no longer passes its checks sends
// nothing at all.
func TestCurrencyLevelingBundleSendsNothingUnlessEveryConversionPasses(t *testing.T) {
	rig, rows, bundle, broker, _ := levelingBundleRig(t)
	prepared, err := rig.engine.PrepareBundle(t.Context(), rpc.TradeProposalPrepareBundleParams{BundleID: bundle.ID, Revision: bundle.Revision})
	if err != nil || !prepared.Accepted {
		t.Fatalf("prepare bundle = %+v, %v", prepared.Blockers, err)
	}
	second := rows[1]
	rig.engine.revalidateForTest = func(_ context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		for _, row := range rows {
			if row.Key == key {
				if key == second.Key {
					return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "stale"}}, nil
				}
				return row, nil, nil
			}
		}
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "gone"}}, nil
	}
	out := submitBundle(t, rig, prepared.BundleRef, bundle)
	if out.Accepted || broker.count() != 0 || !hasBlockerCode(out.Blockers, "bundle_conversion_refused") {
		t.Fatalf("submit with a stale second conversion = %+v (orders %d)", out, broker.count())
	}
	// A wrong revision or an expired bundle is refused before any check.
	stale := bundle
	stale.Revision = "other"
	if out := submitBundle(t, rig, prepared.BundleRef, stale); out.Accepted || !hasBlockerCode(out.Blockers, "prepared_binding_mismatch") {
		t.Fatalf("submit with another revision = %+v", out)
	}
	rig.advance(24 * time.Hour)
	if out := submitBundle(t, rig, prepared.BundleRef, bundle); out.Accepted || !hasBlockerCode(out.Blockers, "prepared_reference_expired") || broker.count() != 0 {
		t.Fatalf("submit a day later = %+v (orders %d)", out, broker.count())
	}
}
