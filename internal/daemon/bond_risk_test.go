package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The held book (bond-risk.md, phase 1): a Treasury measured from
// TreasuryDirect's coupon and maturity at its mark, a bill without issuer
// evidence measured as a zero, and every line it cannot measure named.
func TestMeasureBondPositions(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: new(1e6)})
	cusip := syntheticCUSIP(t, "91282CZZ")
	src := srv.bondEvidenceSupport()
	src.fetchTreasury = func(_ context.Context, c string) ([]treasuryDirectSecurity, error) {
		if c != cusip {
			return nil, nil
		}
		return []treasuryDirectSecurity{{CUSIP: c, SecurityType: "Note", MaturityDate: "2033-09-30T00:00:00", InterestRate: "5"}}, nil
	}
	src.fetchECB = func(context.Context, time.Time) (*ecbEligibleList, error) {
		return &ecbEligibleList{Published: cashSweepDay(now), Assets: map[string]ecbEligibleAsset{}}, nil
	}
	billISIN := syntheticISIN(t, "DE000SYN000")
	linked := syntheticISIN(t, "DE000SYN001")
	src.noteFactorPriced(nil)
	src.mu.Lock()
	src.factorPriced = map[int]bool{4004: true}
	src.mu.Unlock()
	bonds := []rpc.PositionBond{
		{ConID: 4001, Symbol: "UST", Currency: "USD", Class: rpc.BondClassBond, CUSIP: cusip, Issuer: "T 5 09/30/33", Quantity: 10, Mark: 99.14},
		{ConID: 4002, Symbol: "BILL", Currency: "EUR", Class: rpc.BondClassBill, ISIN: billISIN, Maturity: "2027-01-20", Quantity: 10000, Mark: 99.2},
		{ConID: 4003, Symbol: "ODD", Currency: "USD", Class: rpc.BondClassUnresolved, Reason: "contract details unavailable"},
		{ConID: 4004, Symbol: "LINK", Currency: "EUR", Class: rpc.BondClassBond, ISIN: linked, Quantity: 1000, Mark: 101},
	}
	stocks := []rpc.PositionView{
		{ConID: 4001, MarketValueBase: new(8850.0)},
		{ConID: 4002, MarketValueBase: new(9920.0)},
		{ConID: 4004, MarketValueBase: new(1010.0)},
	}
	book := srv.measureBondPositions(context.Background(), bonds, stocks, new(200000.0), "EUR", now)

	ust := bonds[0]
	if ust.RiskUnmeasured != "" || ust.IssuerClass != rpc.BondIssuerGovernment || ust.EvidenceSource != bondEvidenceTreasuryDirect || ust.Maturity != "2033-09-30" ||
		ust.YieldPct == nil || *ust.YieldPct < 5.0 || *ust.YieldPct > 5.3 || ust.ModifiedDuration == nil || *ust.ModifiedDuration < 5.4 || *ust.ModifiedDuration > 6.0 {
		t.Fatalf("treasury = %+v", ust)
	}
	if loss := *ust.RateShockLossBase; loss < 8850*0.05 || loss > 8850*0.065 {
		t.Fatalf("treasury one-point loss = %.2f on 8,850", loss)
	}
	if dv01 := *ust.DV01Base; dv01 < 4.5 || dv01 > 5.5 {
		t.Fatalf("treasury DV01 = %.2f", dv01)
	}
	bill := bonds[1]
	if bill.RiskUnmeasured != "" || bill.IssuerClass != "" || bill.ModifiedDuration == nil || *bill.ModifiedDuration > 0.4 {
		t.Fatalf("bill = %+v", bill)
	}
	if !strings.Contains(bonds[2].RiskUnmeasured, "not classified") || !strings.Contains(bonds[3].RiskUnmeasured, "inflation-linked") {
		t.Fatalf("unmeasured = %q / %q", bonds[2].RiskUnmeasured, bonds[3].RiskUnmeasured)
	}
	if book == nil || book.MarketValueBase != 8850+9920+1010 || book.NonGovernmentBase != 9920+1010 || len(book.Unmeasured) != 2 ||
		book.RateShockLossBase != *ust.RateShockLossBase+*bill.RateShockLossBase || book.PctNLV == nil {
		t.Fatalf("book = %+v", book)
	}
	if srv.measureBondPositions(context.Background(), nil, stocks, new(200000.0), "EUR", now) != nil {
		t.Fatal("an empty book was measured")
	}
}

// A bond buy's preview states its yield, duration and one-point loss.
func TestBondBuyPreviewStatesRateRisk(t *testing.T) {
	rig := newBondRequestRig(t)
	cusip := syntheticCUSIP(t, "91282CZZ")
	rig.line(cusip, "USD", 8101)
	rig.treasury[cusip] = []treasuryDirectSecurity{{CUSIP: cusip, SecurityType: "Note", MaturityDate: cashSweepDay(rig.now).AddDate(7, 0, 0).Format(time.DateOnly) + "T00:00:00", InterestRate: "5"}}
	res, err := rig.preview(rpc.OrderActionBuy, cusip, "BOND", "USD", 10000)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	b := res.Draft.Bond
	if b.YieldPct == nil || *b.YieldPct < 5 || b.ModifiedDuration == nil || *b.ModifiedDuration < 5 || b.RateShockLoss == nil || *b.RateShockLoss < 500 || *b.RateShockLoss > 650 {
		t.Fatalf("rate risk = yield %v duration %v loss %v", b.YieldPct, b.ModifiedDuration, b.RateShockLoss)
	}
}
