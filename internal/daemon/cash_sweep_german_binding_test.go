package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type fakeGermanBillSource struct {
	*fakeBillSource
	issuer    germanBill
	issuerErr error
}

func (f *fakeGermanBillSource) germanBill(context.Context, string, time.Time) (germanBill, error) {
	return f.issuer, f.issuerErr
}

func syntheticGermanBoundSource(now time.Time) (*fakeGermanBillSource, ibkrlib.BondContractDetails) {
	date := cashSweepDay(now).AddDate(0, 0, 100)
	line := synthBondLine(8451, synthDEBill, "EUR", date)
	line.Maturity = ""
	line.IssueDate = ""
	line.CUSIPField = "IBCID8451"
	line.SecIDs = nil
	line.SecType = "BILL"
	src := &fakeGermanBillSource{fakeBillSource: &fakeBillSource{byID: map[string][]ibkrlib.BondContractDetails{synthDEBill: {line}}, quotes: map[int]rpc.BondQuote{8451: synthLiveQuote(99.5)}}, issuer: germanBill{ISIN: synthDEBill, IssueDate: cashSweepDay(now).AddDate(0, 0, -200), MaturityDate: date, FetchedAt: now.Add(-time.Minute)}}
	return src, line
}

func TestCashSweepGermanRequestBoundCandidateAndReview(t *testing.T) {
	now := cashSweepTestNow()
	src, line := syntheticGermanBoundSource(now)
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	policy.Buckets.CashSweep.Currency = map[string]protectionCashSweepCurrency{}
	policy.Buckets.CashSweep.Currency["EUR"] = protectionCashSweepCurrency{Instruments: []string{cashSweepInstrumentDEBubill}, ISINs: []string{synthDEBill}, KeepCash: 5000, MinTranche: 1000, MinMaturityDays: 28, MaxMaturityDays: 182, LadderRungs: 4}
	plan, byCCY := planAndResolve(t, policy, cashSweepTestInput(map[string]float64{"EUR": 60000}), src)
	cp := byCCY["EUR"]
	if cp.side != rpc.CashSweepSideInvest || cp.bill == nil || cp.bill.ISIN != synthDEBill || cp.bill.Maturity != src.issuer.MaturityDate.Format(time.DateOnly) || cp.bill.ResolutionSource != cashSweepResolutionRequestBound || cp.bill.MaturitySource != rpc.CashSweepMaturitySourceGermanIssuer {
		t.Fatalf("unbound candidate: %+v", cp)
	}
	if src.byID[synthDEBill][0].ISIN() != "" || src.byID[synthDEBill][0].Maturity != "" {
		t.Fatal("issuer evidence overwrote broker fields")
	}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cp)
	terms := cashSweepOrderTerms(row)
	if terms.ResolutionSource != cashSweepResolutionRequestBound || terms.ISIN != synthDEBill {
		t.Fatalf("preview lacks query binding: %+v", terms)
	}
	if err := cashSweepCheckGermanPreview(context.Background(), src, []ibkrlib.BondContractDetails{line}, line, *terms, now); err != nil {
		t.Fatal(err)
	}
	changed := *terms
	changed.Maturity = src.issuer.MaturityDate.AddDate(0, 0, 1).Format(time.DateOnly)
	if err := cashSweepCheckGermanPreview(context.Background(), src, []ibkrlib.BondContractDetails{line}, line, changed, now); err == nil {
		t.Fatal("changed date reused review")
	}
	revision := func(p rpc.TradeProposal) string {
		return proposalRevision(rpc.Fingerprint{Key: "synthetic"}, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, []rpc.TradeProposal{p})
	}
	before := revision(row)
	row.CashSweep = rpc.CloneProposalCashSweep(row.CashSweep)
	row.CashSweep.Bill.ResolutionSource = "broker"
	if revision(row) == before {
		t.Fatal("changed identity provenance reused review")
	}
	// IBCID alone is never held issuer identity, even after candidate resolution.
	if _, _, _, err := cashSweepHeldMaturity(src, []ibkrlib.BondContractDetails{line}, line, now); err == nil {
		t.Fatal("request-bound candidate became held evidence")
	}
}

func TestCashSweepGermanBindingRejectsAmbiguityAndContradictions(t *testing.T) {
	now := cashSweepTestNow()
	for name, change := range map[string]func(*fakeGermanBillSource, *[]ibkrlib.BondContractDetails){
		"unreachable": func(s *fakeGermanBillSource, _ *[]ibkrlib.BondContractDetails) {
			s.issuerErr = errors.New("unavailable")
		},
		"foreign issuer": func(s *fakeGermanBillSource, _ *[]ibkrlib.BondContractDetails) { s.issuer.ISIN = synthDEBill2 },
		"stale": func(s *fakeGermanBillSource, _ *[]ibkrlib.BondContractDetails) {
			s.issuer.FetchedAt = now.Add(-treasuryBillUniverseMaxAge - time.Second)
		},
		"future": func(s *fakeGermanBillSource, _ *[]ibkrlib.BondContractDetails) {
			s.issuer.FetchedAt = now.Add(time.Second)
		},
		"not issued": func(s *fakeGermanBillSource, _ *[]ibkrlib.BondContractDetails) {
			s.issuer.IssueDate = cashSweepDay(now).AddDate(0, 0, 1)
		},
		"matured": func(s *fakeGermanBillSource, _ *[]ibkrlib.BondContractDetails) {
			s.issuer.MaturityDate = cashSweepDay(now)
		},
		"maturity contradiction": func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].Maturity = "20260230" },
		"ISIN contradiction": func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) {
			(*l)[0].SecIDs = map[string]string{"ISIN": synthDEBill2}
		},
		"IBCID contradiction":    func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].CUSIPField = "IBCID8452" },
		"currency contradiction": func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].Currency = "USD" },
		"not BILL":               func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].SecType = "BOND" },
		"coupon":                 func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].Coupon = 1 },
		"partial frame":          func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) { (*l)[0].Complete = false },
		"ambiguous query": func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) {
			b := (*l)[0]
			b.ConID++
			*l = append(*l, b)
		},
		"sibling date": func(_ *fakeGermanBillSource, l *[]ibkrlib.BondContractDetails) {
			b := (*l)[0]
			b.Maturity = "20261105"
			*l = append(*l, b)
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, line := syntheticGermanBoundSource(now)
			lines := []ibkrlib.BondContractDetails{line}
			change(src, &lines)
			if _, err := cashSweepGermanCandidate(context.Background(), src, lines, lines[0], synthDEBill, now); err == nil {
				t.Fatal("contradictory issuer/broker evidence admitted")
			}
		})
	}
}

func TestCashSweepHeldGermanExactAllowlistBinding(t *testing.T) {
	now := cashSweepTestNow()
	src, line := syntheticGermanBoundSource(now)
	src.heldLines = map[int][]ibkrlib.BondContractDetails{line.ConID: {line}}
	cfg := protectionCashSweepCurrency{ISINs: []string{synthDEBill}}
	candidate, err := cashSweepHeldGermanBinding(context.Background(), src, src, cfg, line.ConID, now)
	if err != nil || candidate.id != synthDEBill || candidate.resolutionSource != cashSweepResolutionRequestBound {
		t.Fatalf("held binding: %+v %v", candidate, err)
	}
	position := rpc.PositionBond{ConID: line.ConID, Currency: "EUR", Class: rpc.BondClassBill, ISIN: candidate.id, Maturity: candidate.maturity.Format(time.DateOnly), MaturitySource: candidate.maturitySource, MaturitySourceAsOf: candidate.publicFetchedAt, ResolutionSource: candidate.resolutionSource}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{ConID: line.ConID, SecType: "BILL", Currency: "EUR", Quantity: 5000, MarketValue: 4975}}, Bonds: []rpc.PositionBond{position}}
	holdings, blocked := cashSweepClassify(&protectionCashSweepPolicy{Enabled: true}, pos)
	if len(holdings["EUR"]) != 1 || blocked["EUR"] != "" || holdings["EUR"][0].Bond.ResolutionSource != cashSweepResolutionRequestBound {
		t.Fatalf("held projection lost: %+v %+v", holdings, blocked)
	}
	in := cashSweepTestInput(map[string]float64{"EUR": 1000})
	in.Holdings, in.Unclassified = holdings, blocked
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	plan := cashSweepPlanFor(policy, in, now)
	cp := cashSweepCurrencyOf(t, plan, "EUR")
	if cp.side != rpc.CashSweepSideRedeem {
		t.Fatalf("no held EUR redemption: %+v", cp)
	}
	cashSweepResolveRedemption(context.Background(), src, &cp, now)
	if len(cp.blockers) != 0 {
		t.Fatalf("bound EUR redemption blocked: %+v", cp.blockers)
	}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cp)
	terms := cashSweepOrderTerms(row)
	if terms.ResolutionSource != cashSweepResolutionRequestBound || terms.ISIN != synthDEBill || terms.Maturity != position.Maturity {
		t.Fatalf("held EUR preview lost exact binding: %+v", terms)
	}
	if err := cashSweepCheckGermanPreview(context.Background(), src, []ibkrlib.BondContractDetails{line}, line, *terms, now); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*fakeGermanBillSource, *protectionCashSweepCurrency){
		"not allowlisted": func(_ *fakeGermanBillSource, c *protectionCashSweepCurrency) { c.ISINs = nil },
		"remapped query": func(s *fakeGermanBillSource, _ *protectionCashSweepCurrency) {
			b := s.byID[synthDEBill][0]
			b.ConID++
			s.byID[synthDEBill] = []ibkrlib.BondContractDetails{b}
		},
		"unknown competing query": func(s *fakeGermanBillSource, c *protectionCashSweepCurrency) {
			c.ISINs = append(c.ISINs, synthDEBill2)
			s.lineErr = map[string]error{synthDEBill2: errors.New("unavailable")}
		},
		"alias": func(_ *fakeGermanBillSource, c *protectionCashSweepCurrency) { c.ISINs = append(c.ISINs, synthDEBill) },
		"held identity contradiction": func(s *fakeGermanBillSource, _ *protectionCashSweepCurrency) {
			b := line
			b.SecIDs = map[string]string{"ISIN": synthDEBill2}
			s.heldLines[line.ConID] = []ibkrlib.BondContractDetails{b}
		},
		"held maturity contradiction": func(s *fakeGermanBillSource, _ *protectionCashSweepCurrency) {
			b := line
			b.Maturity = "20261105"
			s.heldLines[line.ConID] = []ibkrlib.BondContractDetails{b}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, l := syntheticGermanBoundSource(now)
			s.heldLines = map[int][]ibkrlib.BondContractDetails{l.ConID: {l}}
			cfg := protectionCashSweepCurrency{ISINs: []string{synthDEBill}}
			change(s, &cfg)
			if _, err := cashSweepHeldGermanBinding(context.Background(), s, s, cfg, l.ConID, now); err == nil {
				t.Fatal("unbound held mapping admitted")
			}
		})
	}
}
