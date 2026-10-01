package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestCashSweepHeldPublicMaturityFlowsThroughLadderAndRedemption(t *testing.T) {
	now := cashSweepTestNow()
	src := usBillSource(now)
	line := synthBondLine(8401, synthCUSIP35, "USD", cashSweepDay(now).AddDate(0, 0, 35))
	line.Maturity = ""
	src.heldLines = map[int][]ibkrlib.BondContractDetails{8401: {line}}
	s := &Server{}
	s.cashSweepB.dir = &bondDirectory{fetch: bondFetchOf(func(context.Context, ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, error) {
		return []ibkrlib.BondContractDetails{line}, nil
	})}
	s.cashSweepB.universe = &billUniverse{record: treasuryBillUniverseRecord{FetchedAt: src.at, Bills: src.bills}}
	rows := []rpc.PositionView{{Symbol: "SYNTH", SecType: "BILL", ConID: 8401, Currency: "USD", Quantity: 5, MarketValue: 4975}}
	pos := &rpc.PositionsResult{Stocks: rows, Bonds: s.classifyBondPositions(context.Background(), rows, now)}
	if len(pos.Bonds) != 1 || pos.Bonds[0].Maturity != src.bills[1].MaturityDate || pos.Bonds[0].MaturitySource != rpc.CashSweepMaturitySourceTreasuryDirect || !pos.Bonds[0].MaturitySourceAsOf.Equal(src.at) {
		t.Fatalf("public held maturity lost: %+v", pos.Bonds)
	}
	holdings, blocked := cashSweepClassify(&protectionCashSweepPolicy{Enabled: true}, pos)
	if blocked["USD"] != "" || len(holdings["USD"]) != 1 || holdings["USD"][0].FaceValue != 5000 {
		t.Fatalf("held exposure lost: %+v %+v", holdings, blocked)
	}
	in := cashSweepTestInput(map[string]float64{"USD": 1000})
	in.Holdings, in.Unclassified = holdings, blocked
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	plan := cashSweepPlanFor(policy, in, now)
	cp := cashSweepCurrencyOf(t, plan, "USD")
	if cp.side != rpc.CashSweepSideRedeem {
		t.Fatalf("no redemption: %+v", cp)
	}
	cashSweepResolveRedemption(context.Background(), src, &cp, now)
	if len(cp.blockers) != 0 {
		t.Fatalf("redemption blocked: %+v", cp.blockers)
	}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cp)
	if row.CashSweep.MaturityDate != pos.Bonds[0].Maturity || row.CashSweep.CUSIP != synthCUSIP35 || row.CashSweep.MaturitySource != rpc.CashSweepMaturitySourceTreasuryDirect {
		t.Fatalf("redemption lost provenance: %+v", row.CashSweep)
	}
	terms := cashSweepOrderTerms(row)
	if terms.Maturity != row.CashSweep.MaturityDate || terms.CUSIP != synthCUSIP35 {
		t.Fatalf("preview lost binding: %+v", terms)
	}
	if err := cashSweepCheckReviewedMaturity(src, []ibkrlib.BondContractDetails{line}, line, *terms, now); err != nil {
		t.Fatal(err)
	}
	revision := func(p rpc.TradeProposal) string {
		return proposalRevision(rpc.Fingerprint{Key: "synthetic"}, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, []rpc.TradeProposal{p})
	}
	before := revision(row)
	changed := row
	changed.CashSweep = rpc.CloneProposalCashSweep(row.CashSweep)
	changed.CashSweep.MaturitySourceAsOf = now.Add(time.Hour)
	if revision(changed) != before {
		t.Fatal("receipt refreshed approval")
	}
	changed.CashSweep.MaturityDate = "2026-11-06"
	if revision(changed) == before {
		t.Fatal("changed held date reused approval")
	}
	changed.CashSweep.MaturityDate = row.CashSweep.MaturityDate
	changed.CashSweep.CUSIP = synthCUSIP60
	if revision(changed) == before {
		t.Fatal("changed held identity reused approval")
	}
}

func TestCashSweepHeldMaturityRefusesUnboundEvidence(t *testing.T) {
	now := cashSweepTestNow()
	for name, change := range map[string]func(*fakeBillSource, *[]ibkrlib.BondContractDetails){
		"no public match": func(s *fakeBillSource, _ *[]ibkrlib.BondContractDetails) { s.bills = nil },
		"stale": func(s *fakeBillSource, _ *[]ibkrlib.BondContractDetails) {
			s.at = now.Add(-treasuryBillUniverseMaxAge - time.Second)
		},
		"future": func(s *fakeBillSource, _ *[]ibkrlib.BondContractDetails) { s.at = now.Add(time.Second) },
		"not issued": func(s *fakeBillSource, _ *[]ibkrlib.BondContractDetails) {
			s.bills[1].IssueDate = cashSweepDay(now).AddDate(0, 0, 1).Format(time.DateOnly)
		},
		"conflicting public date": func(s *fakeBillSource, _ *[]ibkrlib.BondContractDetails) {
			b := s.bills[1]
			b.MaturityDate = cashSweepDay(now).AddDate(0, 0, 36).Format(time.DateOnly)
			s.bills = append(s.bills, b)
		},
		"malformed broker": func(_ *fakeBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].Maturity = "2026-02-30" },
		"coupon":           func(_ *fakeBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].Coupon = 1 },
		"wrong sec type":   func(_ *fakeBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].SecType = "BOND" },
		"wrong currency":   func(_ *fakeBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].Currency = "EUR" },
		"sibling maturity": func(_ *fakeBillSource, l *[]ibkrlib.BondContractDetails) {
			b := (*l)[0]
			b.Maturity = "20261106"
			*l = append(*l, b)
		},
		"sibling currency": func(_ *fakeBillSource, l *[]ibkrlib.BondContractDetails) {
			b := (*l)[0]
			b.Currency = "EUR"
			*l = append(*l, b)
		},
		"identifier channel": func(_ *fakeBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].SecIDs["CUSIP"] = synthCUSIP60 },
	} {
		t.Run(name, func(t *testing.T) {
			src := usBillSource(now)
			line := synthBondLine(8401, synthCUSIP35, "USD", cashSweepDay(now).AddDate(0, 0, 35))
			line.Maturity = ""
			lines := []ibkrlib.BondContractDetails{line}
			change(src, &lines)
			if date, _, _, err := cashSweepHeldMaturity(src, lines, lines[0], now); err == nil {
				t.Fatalf("unbound date admitted: %s", date)
			}
		})
	}
}

func TestCashSweepNativeHeldMaturityRejectsIdentifierPrecedence(t *testing.T) {
	now := cashSweepTestNow()
	line := synthBondLine(8401, synthCUSIP35, "USD", cashSweepDay(now).AddDate(0, 0, 35))
	line.SecIDs[ibkrlib.BondIdentifierCUSIP] = synthCUSIP60
	if _, _, _, err := cashSweepHeldMaturity(nil, []ibkrlib.BondContractDetails{line}, line, now); err == nil {
		t.Fatal("native maturity hid conflicting issuer identity")
	}
}
