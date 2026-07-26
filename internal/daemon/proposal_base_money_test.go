package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRiskReductionSetsBaseTwinFromGroupBase(t *testing.T) {
	t.Parallel()
	pct := 40.0
	mvBase := 35000.0
	group := rpc.PositionGroup{
		Underlying:             "MSFT",
		GroupMarketValueBase:   &mvBase,
		GroupMarketValuePctNLV: &pct,
		GroupMarketValue:       40000,
		Stock:                  &rpc.PositionView{Symbol: "MSFT", SecType: "STOCK", Quantity: 100, Mark: 400, Multiplier: 1, Currency: "USD"},
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	prop, ok := riskReductionProposal(policy, status, group, rpc.TradeProposalSourceFingerprints{}, time.Now())
	if !ok {
		t.Fatal("risk proposal missing")
	}
	if prop.RiskExcessNotionalBase == nil {
		t.Fatal("base twin missing despite GroupMarketValueBase")
	}
	// Same excess share as the instrument-ccy figure (15000 of 40000),
	// applied to the base market value: 35000 * 15/40.
	if want := 13125.0; math.Abs(*prop.RiskExcessNotionalBase-want) > 1e-9 {
		t.Fatalf("base twin = %v, want %v", *prop.RiskExcessNotionalBase, want)
	}

	group.GroupMarketValueBase = nil
	prop, ok = riskReductionProposal(policy, status, group, rpc.TradeProposalSourceFingerprints{}, time.Now())
	if !ok {
		t.Fatal("risk proposal missing without base")
	}
	if prop.RiskExcessNotionalBase != nil {
		t.Fatalf("base twin = %v; want nil when group base is unavailable", *prop.RiskExcessNotionalBase)
	}
}

func TestProposalCountsBaseTwins(t *testing.T) {
	t.Parallel()
	theta1, theta2, risk := 100.0, 50.0, 13125.0
	proposals := []rpc.TradeProposal{
		{Bucket: rpc.TradeProposalBucketThetaHygiene, ThetaPerDay: 120, ThetaPerDayBase: &theta1, Contract: rpc.ContractParams{Currency: "USD"}},
		{Bucket: rpc.TradeProposalBucketThetaHygiene, ThetaPerDay: 60, ThetaPerDayBase: &theta2, Contract: rpc.ContractParams{Currency: "USD"}},
		{Bucket: rpc.TradeProposalBucketRiskReduction, RiskExcessNotional: 15000, RiskExcessCurrency: "USD", RiskExcessNotionalBase: &risk},
	}
	counts := proposalCounts(proposals, "eur")
	if counts.ThetaPerDayBase == nil || *counts.ThetaPerDayBase != 150 {
		t.Fatalf("theta base aggregate = %v, want 150", counts.ThetaPerDayBase)
	}
	if counts.RiskReductionExcessNotionalBase == nil || *counts.RiskReductionExcessNotionalBase != 13125 {
		t.Fatalf("risk base aggregate = %v, want 13125", counts.RiskReductionExcessNotionalBase)
	}
	if counts.BaseCurrency != "EUR" {
		t.Fatalf("base currency = %q, want EUR", counts.BaseCurrency)
	}
	if counts.ThetaPerDayCurrency != "USD" {
		t.Fatalf("theta currency = %q, want USD", counts.ThetaPerDayCurrency)
	}
}

func TestProposalCountsOmitsBaseTwinWhenContributorMissing(t *testing.T) {
	t.Parallel()
	theta := 100.0
	proposals := []rpc.TradeProposal{
		{Bucket: rpc.TradeProposalBucketThetaHygiene, ThetaPerDay: 120, ThetaPerDayBase: &theta, Contract: rpc.ContractParams{Currency: "USD"}},
		{Bucket: rpc.TradeProposalBucketThetaHygiene, ThetaPerDay: 60, Contract: rpc.ContractParams{Currency: "USD"}},
	}
	counts := proposalCounts(proposals, "EUR")
	if counts.ThetaPerDayBase != nil {
		t.Fatalf("theta base aggregate = %v; want nil when a contributor lacks its twin", *counts.ThetaPerDayBase)
	}
	if counts.ThetaPerDay != 180 {
		t.Fatalf("raw theta sum = %v, want 180 (legacy field keeps the raw sum)", counts.ThetaPerDay)
	}
	if counts.BaseCurrency != "" {
		t.Fatalf("base currency = %q; want omitted when no base aggregate is served", counts.BaseCurrency)
	}
}

func TestProposalCountsMixedThetaCurrencyKeepsRawSum(t *testing.T) {
	t.Parallel()
	proposals := []rpc.TradeProposal{
		{Bucket: rpc.TradeProposalBucketThetaHygiene, ThetaPerDay: 120, Contract: rpc.ContractParams{Currency: "USD"}},
		{Bucket: rpc.TradeProposalBucketThetaHygiene, ThetaPerDay: 60, Contract: rpc.ContractParams{Currency: "EUR"}},
	}
	counts := proposalCounts(proposals, "")
	if counts.ThetaPerDay != 180 {
		t.Fatalf("raw theta sum = %v, want 180 — zeroing on MIX would read as nothing-pending", counts.ThetaPerDay)
	}
	if counts.ThetaPerDayCurrency != "" {
		t.Fatalf("theta currency = %q; want omitted on mixed currencies", counts.ThetaPerDayCurrency)
	}
}
