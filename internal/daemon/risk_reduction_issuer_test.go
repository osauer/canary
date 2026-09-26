package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func issuerTestBook(shares float64) (*rpc.AccountResult, *rpc.PositionsResult) {
	mv := shares * 100
	pct := mv / 100000 * 100
	stock := rpc.PositionView{Symbol: "AAA", SecType: "STK", ConID: 11, Exchange: "SMART", Currency: "USD", Quantity: shares, Multiplier: 1,
		Mark: 100, MarketValue: mv, MarketValueBase: &mv}
	pos := &rpc.PositionsResult{
		Stocks:       []rpc.PositionView{stock},
		ByUnderlying: []rpc.PositionGroup{{Underlying: "AAA", Stock: &stock, GroupMarketValue: mv, GroupMarketValueBase: &mv, GroupMarketValuePctNLV: &pct, GroupDollarDeltaBase: &mv}},
		Portfolio:    &rpc.PositionsPortfolio{BaseCurrency: "USD"},
	}
	return &rpc.AccountResult{AccountID: "DU1234567", NetLiquidation: 100000, BaseCurrency: "USD"}, pos
}

// The risk-reduction bucket triggers at rule 1's act level and trims back to
// its watch level on the same worst-case-loss measure: 45% of NLV in one
// stock sells 150 shares to reach 30%, and 35% (watch) proposes nothing.
func TestRiskReductionTrimsIssuerBackToTheWatchLevel(t *testing.T) {
	now := time.Date(2026, 9, 26, 15, 0, 0, 0, time.UTC)
	engine := &proposalEngine{server: &Server{}}
	policy := defaultProtectionPolicy()
	policy.Buckets.RiskReduction.MaxOrderNotional = 1e9
	_, status := (*protectionPolicyManager)(nil).Active()

	acct, pos := issuerTestBook(450)
	props := engine.riskReductionProposals(context.Background(), policy, status, acct, pos, rpc.TradeProposalSourceFingerprints{}, now)
	if len(props) != 1 {
		t.Fatalf("45%% issuer produced %d proposals, want 1", len(props))
	}
	p := props[0]
	if p.Quantity != 150 || p.Action != rpc.OrderActionSell || p.Bucket != rpc.TradeProposalBucketRiskReduction ||
		p.IssuerLossPctNLV == nil || *p.IssuerLossPctNLV != 45 || p.IssuerTargetPctNLV == nil || *p.IssuerTargetPctNLV != 30 ||
		p.IssuerLossAfterPctNLV == nil || *p.IssuerLossAfterPctNLV != 30 || !strings.Contains(p.Reason, "returns it to the 30% watch level") {
		t.Fatalf("trim = %+v", p)
	}
	if p.RiskExcessNotionalBase == nil || *p.RiskExcessNotionalBase != 15000 {
		t.Fatalf("excess over the watch level = %v, want 15,000", p.RiskExcessNotionalBase)
	}

	// The order cap still bounds one order; the rest waits.
	policy.Buckets.RiskReduction.MaxOrderNotional = 10000
	props = engine.riskReductionProposals(context.Background(), policy, status, acct, pos, rpc.TradeProposalSourceFingerprints{}, now)
	if len(props) != 1 || props[0].Quantity != 100 || !strings.Contains(strings.Join(props[0].Details, " "), "max_order_notional") {
		t.Fatalf("capped trim = %+v", props)
	}

	acct, pos = issuerTestBook(350)
	if props := engine.riskReductionProposals(context.Background(), policy, status, acct, pos, rpc.TradeProposalSourceFingerprints{}, now); len(props) != 0 {
		t.Fatalf("an issuer at watch (35%%) must not be trimmed: %+v", props)
	}
}
