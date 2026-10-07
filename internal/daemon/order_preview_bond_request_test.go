package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// bondRequestRig previews bonds named by identifier against synthetic broker
// lines and synthetic issuer sources (internal-docs/design/bond-orders.md).
type bondRequestRig struct {
	srv          *Server
	now          time.Time
	lines        map[string]ibkrlib.BondContractDetails
	lookups      []ibkrlib.BondContractRequest
	drafts       []rpc.OrderDraft
	marginFactor float64
	treasury     map[string][]treasuryDirectSecurity
	ecb          map[string]ecbEligibleAsset
	figi         map[string][]openFIGIRecord
}

func newBondRequestRig(t *testing.T) *bondRequestRig {
	t.Helper()
	rig := &bondRequestRig{now: time.Date(2026, 5, 27, 14, 0, 0, 0, time.UTC), lines: map[string]ibkrlib.BondContractDetails{}, marginFactor: 0.05,
		treasury: map[string][]treasuryDirectSecurity{}, ecb: map[string]ecbEligibleAsset{}, figi: map[string][]openFIGIRecord{}}
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: new(1e6)})
	srv.now = func() time.Time { return rig.now }
	srv.orderBondLookupForTest = func(_ context.Context, r ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, error) {
		rig.lookups = append(rig.lookups, r)
		line, ok := rig.lines[r.ID]
		if !ok {
			return nil, ibkrlib.ErrBondContractNotFound
		}
		return []ibkrlib.BondContractDetails{line}, nil
	}
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 98.40, 98.44
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	srv.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		if action == rpc.OrderActionSell {
			return rpc.OrderPositionImpact{Before: 50000, After: 50000 - float64(qty), Effect: rpc.OrderPositionEffectReduce}, nil
		}
		return rpc.OrderPositionImpact{Before: 0, After: float64(qty), Effect: rpc.OrderPositionEffectOpen}, nil
	}
	srv.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		rig.drafts = append(rig.drafts, d)
		before := 1000.0
		after := before + float64(d.Quantity)*d.Bond.FacePerUnit*d.LimitPrice/100*rig.marginFactor
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true,
			Margin: &rpc.OrderMarginImpact{Currency: d.Contract.Currency, InitialMarginBefore: &before, InitialMarginAfter: &after, CommissionCurrency: "USD", MaxCommission: new(1.0)}}, nil
	}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: rig.now}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
	}
	src := srv.bondEvidenceSupport()
	src.fetchTreasury = func(_ context.Context, cusip string) ([]treasuryDirectSecurity, error) {
		return rig.treasury[cusip], nil
	}
	src.fetchECB = func(context.Context, time.Time) (*ecbEligibleList, error) {
		return &ecbEligibleList{Published: cashSweepDay(rig.now), Assets: rig.ecb}, nil
	}
	src.fetchFIGI = func(_ context.Context, isin string) ([]openFIGIRecord, error) { return rig.figi[isin], nil }
	rig.srv = srv
	return rig
}

// line adds a broker line named by id: IBKR's restricted answer, with no
// maturity, no coupon and the request's currency, on the line's own hours.
func (r *bondRequestRig) line(id, ccy string, conID int) *ibkrlib.BondContractDetails {
	line := synthBondLine(conID, id, ccy, cashSweepDay(r.now).AddDate(3, 0, 0))
	line.SecType, line.Maturity, line.IssueDate = "BOND", "", ""
	if ccy == "USD" {
		line.MinSize, line.SizeIncrement = 1, 1
	}
	r.lines[id] = line
	got := r.lines[id]
	return &got
}

func (r *bondRequestRig) setLine(id string, line ibkrlib.BondContractDetails) { r.lines[id] = line }

func (r *bondRequestRig) preview(action, id, secType, ccy string, face float64) (*rpc.OrderPreviewResult, error) {
	return r.srv.previewOrder(context.Background(), rpc.OrderPreviewParams{Action: action,
		Contract: rpc.ContractParams{SecType: secType, Currency: ccy}, BondOrder: &rpc.OrderBondRequest{Identifier: id, Face: face}})
}

func refusalCodes(err error) []string {
	var codes []string
	for _, b := range previewFailureBlockers(err) {
		codes = append(codes, b.Code)
	}
	return codes
}

// A US Treasury note named by CUSIP: TreasuryDirect vouches for it, the face
// becomes order units of 1,000, the patient limit sits on the line's tick,
// and the value for the order cap adds one year of coupon.
func TestBondOrderByIdentifierBuysATreasuryNote(t *testing.T) {
	rig := newBondRequestRig(t)
	cusip := syntheticCUSIP(t, "91282CZZ")
	rig.line(cusip, "USD", 8101)
	maturity := cashSweepDay(rig.now).AddDate(7, 0, 0)
	rig.treasury[cusip] = []treasuryDirectSecurity{{CUSIP: cusip, SecurityType: "Note", Type: "Note", MaturityDate: maturity.Format(time.DateOnly) + "T00:00:00", InterestRate: "5.000000"}}

	res, err := rig.preview(rpc.OrderActionBuy, cusip, "BOND", "USD", 25000)
	if err != nil {
		t.Fatalf("preview: %v (%v)", err, refusalCodes(err))
	}
	d := res.Draft
	if d.Contract.ConID != 8101 || d.Contract.SecType != "BOND" || d.Contract.Currency != "USD" || d.Quantity != 25 || d.OrderType != rpc.OrderTypeLMT ||
		d.TIF != rpc.OrderTIFDay || d.Strategy != rpc.OrderStrategyPatientLimit || d.LimitPrice != 98.42 {
		t.Fatalf("draft = %+v", d)
	}
	b := d.Bond
	if b == nil || b.Instrument != rpc.OrderBondInstrumentByIdentifier || b.FacePerUnit != 1000 || b.FaceValue != 25000 || b.IssuerClass != rpc.BondIssuerGovernment ||
		b.EvidenceSource != bondEvidenceTreasuryDirect || b.Issuer != "United States Treasury" || b.Maturity != maturity.Format(time.DateOnly) ||
		b.Coupon == nil || *b.Coupon != 5 || b.AccruedBound != 1250 || b.CUSIP != cusip {
		t.Fatalf("bond terms = %+v", b)
	}
	if want := 25000*98.42/100 + 1250; math.Abs(res.Notional-want) > 1e-6 {
		t.Fatalf("notional = %v, want %v (clean value plus the accrued bound)", res.Notional, want)
	}
	if !res.TokenMinted || res.PreviewToken == "" {
		t.Fatalf("no token: %+v", res)
	}
	if len(rig.lookups) != 1 || rig.lookups[0].IDType != ibkrlib.BondIdentifierCUSIP || rig.lookups[0].SecTypes[0] != "BOND" {
		t.Fatalf("lookups = %+v", rig.lookups)
	}
}

// A Bund named by ISIN is a government bond on the ECB's list; an
// ECB-listed corporate is investment grade. EUR face counts one unit per
// euro and must sit on the line's size grid.
func TestBondOrderByIdentifierAdmitsECBListedBonds(t *testing.T) {
	rig := newBondRequestRig(t)
	bund := syntheticISIN(t, "DE000SYN000")
	corp := syntheticISIN(t, "XS000SYN000")
	rig.line(bund, "EUR", 8201)
	rig.line(corp, "EUR", 8202)
	maturity := cashSweepDay(rig.now).AddDate(10, 0, 0)
	rig.ecb[bund] = ecbEligibleAsset{ISIN: bund, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerName: "Central government: Synthetic Republic", IssuerGroup: "IG2", Maturity: maturity, Coupon: 2.5}
	rig.ecb[corp] = ecbEligibleAsset{ISIN: corp, Type: "AT02", Denomination: "EUR", CouponDefinition: "CD4", IssuerName: "SYNTH CORP", IssuerGroup: "IG3", Maturity: maturity, Coupon: 3}

	res, err := rig.preview(rpc.OrderActionBuy, bund, "BOND", "EUR", 10000)
	if err != nil {
		t.Fatalf("bund: %v (%v)", err, refusalCodes(err))
	}
	if b := res.Draft.Bond; res.Draft.Quantity != 10000 || b.FacePerUnit != 1 || b.IssuerClass != rpc.BondIssuerGovernment || b.Issuer != "Synthetic Republic" ||
		b.EvidenceSource != bondEvidenceECB || b.AccruedBound != 250 {
		t.Fatalf("bund draft = %+v terms %+v", res.Draft, res.Draft.Bond)
	}
	res, err = rig.preview(rpc.OrderActionBuy, corp, "BOND", "EUR", 5000)
	if err != nil || res.Draft.Bond.IssuerClass != rpc.BondIssuerInvestmentGrade {
		t.Fatalf("corporate: %v (%v) %+v", err, refusalCodes(err), res)
	}
	if _, err := rig.preview(rpc.OrderActionBuy, bund, "BOND", "EUR", 10500); err == nil || !slices.Contains(refusalCodes(err), previewBondOrderInvalidCode) ||
		!strings.Contains(err.Error(), "step is 1,000 EUR") {
		t.Fatalf("off-grid face: %v %v", err, refusalCodes(err))
	}
}

// Each refusal of the admission rules, with the code it carries.
func TestBondOrderByIdentifierRefusals(t *testing.T) {
	cases := map[string]struct {
		setup  func(*testing.T, *bondRequestRig) (action, id, ccy string, face float64)
		code   string
		phrase string
	}{
		"a corporate off the ECB list": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "US000SYN000")
			r.line(id, "USD", 8301)
			return rpc.OrderActionBuy, id, "USD", 10000
		}, previewBondIssuerUnprovenCode, "not on the ECB's list"},
		"an inflation-linked line": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN001")
			line := r.line(id, "EUR", 8302)
			line.FactorPriced = true
			r.setLine(id, *line)
			r.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: r.now.AddDate(10, 0, 0), Coupon: 0.1}
			return rpc.OrderActionBuy, id, "EUR", 10000
		}, previewBondIssuerUnprovenCode, "factored principal"},
		"beyond the 30-year limit": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN002")
			r.line(id, "EUR", 8303)
			r.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: cashSweepDay(r.now).AddDate(31, 0, 0), Coupon: 3}
			return rpc.OrderActionBuy, id, "EUR", 10000
		}, previewRiskLimitCode, "max_bond_maturity_years"},
		"the wrong currency": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN003")
			r.line(id, "USD", 8304)
			r.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: r.now.AddDate(5, 0, 0), Coupon: 3}
			return rpc.OrderActionBuy, id, "USD", 10000
		}, previewBondIssuerUnprovenCode, "a EUR bond"},
		"a broker maturity that disagrees": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN004")
			line := r.line(id, "EUR", 8305)
			line.Maturity = cashSweepDay(r.now).AddDate(6, 0, 0).Format("20060102")
			r.setLine(id, *line)
			r.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: cashSweepDay(r.now).AddDate(5, 0, 0), Coupon: 3}
			return rpc.OrderActionBuy, id, "EUR", 10000
		}, previewBondIssuerUnprovenCode, "differs"},
		"a broker coupon that disagrees": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN011")
			line := r.line(id, "EUR", 8312)
			line.Coupon = 4
			r.setLine(id, *line)
			r.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: r.now.AddDate(5, 0, 0), Coupon: 3}
			return rpc.OrderActionBuy, id, "EUR", 10000
		}, previewBondIssuerUnprovenCode, "coupon 4"},
		"maturing within a week": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN005")
			r.line(id, "EUR", 8306)
			r.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT03", Denomination: "EUR", CouponDefinition: "CD1", IssuerGroup: "IG2", Maturity: cashSweepDay(r.now).AddDate(0, 0, 5)}
			return rpc.OrderActionBuy, id, "EUR", 10000
		}, previewBondIssuerUnprovenCode, "within 7 days"},
		"a face of half a USD unit": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticCUSIP(t, "91282CZY")
			r.line(id, "USD", 8307)
			return rpc.OrderActionBuy, id, "USD", 1500
		}, previewBondOrderInvalidCode, "whole number of order units of 1,000 USD"},
		"a line naming another ISIN": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN006")
			line := r.line(id, "EUR", 8308)
			line.CUSIPField = syntheticISIN(t, "DE000SYN007")
			r.setLine(id, *line)
			return rpc.OrderActionBuy, id, "EUR", 10000
		}, previewContractUnresolvedCode, "contradictory"},
		"a unit the broker's margin contradicts": {func(t *testing.T, r *bondRequestRig) (string, string, string, float64) {
			id := syntheticISIN(t, "DE000SYN008")
			r.line(id, "EUR", 8309)
			r.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: r.now.AddDate(5, 0, 0), Coupon: 3}
			r.marginFactor = 1000
			return rpc.OrderActionBuy, id, "EUR", 10000
		}, previewBondUnitMismatchCode, "outside the band"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newBondRequestRig(t)
			action, id, ccy, face := tc.setup(t, rig)
			_, err := rig.preview(action, id, "BOND", ccy, face)
			if err == nil {
				t.Fatal("previewed")
			}
			if tc.code != "" && (!slices.Contains(refusalCodes(err), tc.code) || !strings.Contains(err.Error(), tc.phrase)) {
				t.Fatalf("err = %v, codes %v; want %s with %q", err, refusalCodes(err), tc.code, tc.phrase)
			}
			if len(rig.drafts) > 0 && tc.code != previewBondUnitMismatchCode {
				t.Fatalf("a refused buy reached the broker's WhatIf: %+v", rig.drafts)
			}
		})
	}
}

// A sale of a held line needs no issuer evidence (it only reduces the
// position) and carries no accrued interest bound; a sale that would open a
// short is refused.
func TestBondOrderByIdentifierSellsAHeldLine(t *testing.T) {
	rig := newBondRequestRig(t)
	id := syntheticISIN(t, "XS000SYN009")
	rig.line(id, "EUR", 8401)
	src := rig.srv.bondEvidenceSupport()
	src.fetchECB = func(context.Context, time.Time) (*ecbEligibleList, error) { return nil, errors.New("HTTP 503") }

	res, err := rig.preview(rpc.OrderActionSell, id, "BOND", "EUR", 20000)
	if err != nil {
		t.Fatalf("sale: %v (%v)", err, refusalCodes(err))
	}
	if b := res.Draft.Bond; res.Draft.Action != rpc.OrderActionSell || res.Draft.Quantity != 20000 || b.IssuerClass != "" || b.AccruedBound != 0 || res.Draft.LimitPrice != 98.42 {
		t.Fatalf("sale draft = %+v terms %+v", res.Draft, b)
	}
	rig.srv.orderPreviewPositionImpact = fixedPreviewPosition(0, -20000, rpc.OrderPositionEffectOpenShort)
	if _, err := rig.preview(rpc.OrderActionSell, id, "BOND", "EUR", 20000); err == nil || !strings.Contains(err.Error(), "short") {
		t.Fatalf("short sale: %v", err)
	}
}

// The request admits one shape, and bond terms never come from the wire.
func TestBondOrderByIdentifierRequestShape(t *testing.T) {
	rig := newBondRequestRig(t)
	id := syntheticISIN(t, "DE000SYN000")
	base := func() rpc.OrderPreviewParams {
		return rpc.OrderPreviewParams{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{SecType: "BOND", Currency: "EUR"}, BondOrder: &rpc.OrderBondRequest{Identifier: id, Face: 10000}}
	}
	for name, change := range map[string]func(*rpc.OrderPreviewParams){
		"a quantity":        func(p *rpc.OrderPreviewParams) { p.Quantity = 10 },
		"an explicit limit": func(p *rpc.OrderPreviewParams) { p.LimitPrice = new(98.0) },
		"GTC":               func(p *rpc.OrderPreviewParams) { p.TIF = rpc.OrderTIFGTC },
		"outside RTH":       func(p *rpc.OrderPreviewParams) { p.OutsideRTH = true },
		"a stock type":      func(p *rpc.OrderPreviewParams) { p.Contract.SecType = "STK" },
		"no currency":       func(p *rpc.OrderPreviewParams) { p.Contract.Currency = "" },
		"a JPY bond":        func(p *rpc.OrderPreviewParams) { p.Contract.Currency = "JPY" },
		"a contract id":     func(p *rpc.OrderPreviewParams) { p.Contract.ConID = 8201 },
		"a bad identifier":  func(p *rpc.OrderPreviewParams) { p.BondOrder.Identifier = "DE000SYN0000" },
		"no face":           func(p *rpc.OrderPreviewParams) { p.BondOrder.Face = 0 },
		"a replacement":     func(p *rpc.OrderPreviewParams) { p.ReplaceID = "ref-1" },
	} {
		p := base()
		change(&p)
		if _, err := rig.srv.previewOrder(context.Background(), p); err == nil {
			t.Errorf("%s: previewed", name)
		}
	}
	if len(rig.lookups) != 0 {
		t.Fatalf("a refused request reached the broker: %+v", rig.lookups)
	}
	raw := []byte(`{"action":"BUY","contract":{"sec_type":"BOND","currency":"EUR"},"bond_order":{"identifier":"` + id + `","face":10000},` +
		`"bond":{"instrument":"us_tbill","face_per_unit":1,"issuer_class":"government","trading_cap_exempt_up_to_base":1e9}}`)
	var decoded rpc.OrderPreviewParams
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Bond != nil || decoded.BondOrder == nil || decoded.BondOrder.Face != 10000 {
		t.Fatalf("decoded = %+v, %v: bond terms crossed the wire", decoded, err)
	}
}

// The order risk authority, which also runs at place and at the wire guard,
// checks a bond buy's maturity against the limit in force; a sale is never
// limited, and a buy without terms cannot pass.
func TestOrderRiskAuthorityChecksBondBuyMaturity(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	limits := testOrderLimits(100000)
	limits.AsOf = now
	auth := orderNotionalAuthority{QuoteNotional: 5000, ContractCurrency: "EUR", BaseNotional: 5000, BaseCurrency: "EUR", BasePerContract: 1, EvidenceAt: now, Source: orderFXSourceIdentity}
	buy := func(maturity string, terms bool) rpc.OrderDraft {
		d := rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{SecType: "BOND", Currency: "EUR", ConID: 8201}, Quantity: 5000}
		if terms {
			d.Bond = &rpc.OrderBondTerms{Instrument: rpc.OrderBondInstrumentByIdentifier, Maturity: maturity, FacePerUnit: 1}
		}
		return d
	}
	open := rpc.OrderPositionImpact{Before: 0, After: 5000, Effect: rpc.OrderPositionEffectOpen}
	limit := now.AddDate(limits.MaxBondMaturityYears, 0, 0).Format(time.DateOnly)
	if err := validateOrderRiskAuthority(limits, buy(limit, true), open, auth, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err != nil {
		t.Fatalf("a buy maturing on the limit was refused: %v", err)
	}
	beyond := now.AddDate(limits.MaxBondMaturityYears, 0, 1).Format(time.DateOnly)
	if err := validateOrderRiskAuthority(limits, buy(beyond, true), open, auth, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil || !strings.Contains(err.Error(), "max_bond_maturity_years") {
		t.Fatalf("a buy beyond the limit: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, buy("", false), open, auth, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil {
		t.Fatal("a bond buy without terms passed")
	}
	sale := buy(beyond, true)
	sale.Action = rpc.OrderActionSell
	reduce := rpc.OrderPositionImpact{Before: 10000, After: 5000, Effect: rpc.OrderPositionEffectReduce}
	if err := validateOrderRiskAuthority(limits, sale, reduce, auth, "EUR", protectiveExitInventory{Current: true}, deltaReductionEvidence{}); err != nil {
		t.Fatalf("a sale was limited by maturity: %v", err)
	}
	// Two sales of one held face would sell short together: the open-order
	// list must be current and leave room for this sale beside the others.
	if err := validateOrderRiskAuthority(limits, sale, reduce, auth, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil || !strings.Contains(err.Error(), "open-order list") {
		t.Fatalf("a sale without the open-order list: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, sale, reduce, auth, "EUR", protectiveExitInventory{Current: true, OtherWorkingSameSide: 5000}, deltaReductionEvidence{}); err != nil {
		t.Fatalf("a sale that fits beside a working one was refused: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, sale, reduce, auth, "EUR", protectiveExitInventory{Current: true, OtherWorkingSameSide: 5001}, deltaReductionEvidence{}); err == nil || !strings.Contains(err.Error(), "already working") {
		t.Fatalf("a sale beyond the held face with working sales: %v", err)
	}
}

// The order cap judges a bond buy at its clean value plus the accrued
// interest bound: a buy whose clean value fits under the cap but whose bound
// does not is refused, and one with room for both passes.
func TestBondOrderByIdentifierCapIncludesAccruedInterest(t *testing.T) {
	rig := newBondRequestRig(t)
	bund := syntheticISIN(t, "DE000SYN000")
	rig.line(bund, "EUR", 8201)
	rig.ecb[bund] = ecbEligibleAsset{ISIN: bund, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: rig.now.AddDate(5, 0, 0), Coupon: 3}
	// 10,000 face at 98.42 is 9,842 EUR clean; one year of a 3% coupon adds 300.
	setTestOrderLimits(rig.srv, func(o *risk.ConstitutionOrderLimits) {
		o.MaxOrderFloorBase, o.MaxOrderCeilingBase = new(10000.0), new(10000.0)
	})
	_, err := rig.preview(rpc.OrderActionBuy, bund, "BOND", "EUR", 10000)
	if err == nil || !slices.Contains(refusalCodes(err), previewRiskLimitCode) || !strings.Contains(err.Error(), "order notional 10,142 EUR exceeds the order cap in force 10,000 EUR") {
		t.Fatalf("clean value under the cap, bound over it: %v %v", err, refusalCodes(err))
	}
	setTestOrderLimits(rig.srv, func(o *risk.ConstitutionOrderLimits) {
		o.MaxOrderFloorBase, o.MaxOrderCeilingBase = new(10200.0), new(10200.0)
	})
	if res, err := rig.preview(rpc.OrderActionBuy, bund, "BOND", "EUR", 10000); err != nil || res.Notional != 10142 {
		t.Fatalf("room for both: %v %v", err, res)
	}
}

// IBKR's factor notice need not accompany every lookup: once a contract was
// flagged, a later lookup without the notice still refuses the buy.
func TestBondOrderByIdentifierRemembersAFactorPricedLine(t *testing.T) {
	rig := newBondRequestRig(t)
	id := syntheticISIN(t, "FR000SYN000")
	line := rig.line(id, "EUR", 8501)
	rig.ecb[id] = ecbEligibleAsset{ISIN: id, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: rig.now.AddDate(10, 0, 0), Coupon: 0.1}
	flagged := *line
	flagged.FactorPriced = true
	rig.setLine(id, flagged)
	if _, err := rig.preview(rpc.OrderActionBuy, id, "BOND", "EUR", 10000); err == nil || !strings.Contains(err.Error(), "factored principal") {
		t.Fatalf("flagged lookup: %v", err)
	}
	rig.setLine(id, *line)
	if _, err := rig.preview(rpc.OrderActionBuy, id, "BOND", "EUR", 10000); err == nil || !strings.Contains(err.Error(), "factored principal") {
		t.Fatalf("a later lookup without the notice was admitted: %v", err)
	}
}

// A sale sees every other working sale of the line in the broker's open
// orders: together they may not sell more than the held face.
func TestBondOrderByIdentifierSaleCountsWorkingSales(t *testing.T) {
	rig := newBondRequestRig(t)
	id := syntheticISIN(t, "XS000SYN010")
	rig.line(id, "EUR", 8601)
	working := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: "DU1234567", ConID: 8601, SecType: "BOND", Currency: "EUR",
		Action: rpc.OrderActionSell, OrderType: "LMT", TotalQuantity: 40000, Remaining: 40000, LimitPrice: 98.4, Status: "Submitted"}
	rig.srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: rig.now, Orders: []ibkrlib.OrderLifecycleEvent{working}}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
	}
	// 50,000 held, 40,000 already working: 20,000 more would sell short.
	if _, err := rig.preview(rpc.OrderActionSell, id, "BOND", "EUR", 20000); err == nil || !strings.Contains(err.Error(), "already working") {
		t.Fatalf("sale beside a working sale: %v", err)
	}
	if _, err := rig.preview(rpc.OrderActionSell, id, "BOND", "EUR", 10000); err != nil {
		t.Fatalf("sale that fits: %v (%v)", err, refusalCodes(err))
	}
}

// Owner decision 2026-10-07 08:27 CEST: while risk-policy.toml does not write
// max_bond_maturity_years, bond buys are refused and every other order is
// judged as usual; an exit or a stop is never blocked by it.
func TestMissingBondMaturityKeyRefusesBondBuysOnly(t *testing.T) {
	rig := newBondRequestRig(t)
	bund := syntheticISIN(t, "DE000SYN000")
	rig.line(bund, "EUR", 8201)
	rig.ecb[bund] = ecbEligibleAsset{ISIN: bund, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerGroup: "IG2", Maturity: rig.now.AddDate(5, 0, 0), Coupon: 3}
	setTestOrderLimits(rig.srv, func(o *risk.ConstitutionOrderLimits) { o.MaxBondMaturityYears = nil })
	if _, err := rig.preview(rpc.OrderActionBuy, bund, "BOND", "EUR", 10000); err == nil || !strings.Contains(err.Error(), "max_bond_maturity_years is not set") {
		t.Fatalf("bond buy with the key missing: %v", err)
	}
	if _, err := rig.preview(rpc.OrderActionSell, bund, "BOND", "EUR", 10000); err != nil {
		t.Fatalf("bond sale with the key missing: %v (%v)", err, refusalCodes(err))
	}
	limits := rig.srv.orderLimitsInForce("EUR")
	stock := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 10, Contract: rpc.ContractParams{Symbol: "AAA", SecType: "STK", Currency: "EUR"}}
	auth := orderNotionalAuthority{QuoteNotional: 1000, ContractCurrency: "EUR", BaseNotional: 1000, BaseCurrency: "EUR", BasePerContract: 1, EvidenceAt: rig.now, Source: orderFXSourceIdentity}
	if err := validateOrderRiskAuthority(limits, stock, rpc.OrderPositionImpact{Before: 0, After: 10, Effect: rpc.OrderPositionEffectOpen}, auth, "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err != nil && strings.Contains(err.Error(), "max_bond_maturity_years") {
		t.Fatalf("a stock order was refused for the bond key: %v", err)
	}
	if !limits.Complete || !limits.BondMaturityUnset {
		t.Fatalf("limits = %+v, want complete with the bond limit unset", limits)
	}
}

// IBKR's bond frames carry no symbol (live read 2026-10-07: every preview
// failed with "contract symbol is required for market data"). The order's
// contract falls back to the CUSIP, then the ISIN, as the sweep's does.
func TestBondOrderByIdentifierQuotesALineWithoutSymbol(t *testing.T) {
	rig := newBondRequestRig(t)
	cusip := syntheticCUSIP(t, "91282CZZ")
	line := rig.line(cusip, "USD", 8701)
	line.Symbol = ""
	rig.setLine(cusip, *line)
	rig.treasury[cusip] = []treasuryDirectSecurity{{CUSIP: cusip, SecurityType: "Note", Type: "Note", MaturityDate: cashSweepDay(rig.now).AddDate(7, 0, 0).Format(time.DateOnly) + "T00:00:00", InterestRate: "5"}}
	var quoted rpc.ContractParams
	quote := rig.srv.orderPreviewQuote
	rig.srv.orderPreviewQuote = func(ctx context.Context, c rpc.ContractParams, d time.Duration) (rpc.OrderQuoteSnapshot, error) {
		quoted = c
		if strings.TrimSpace(c.Symbol) == "" {
			return rpc.OrderQuoteSnapshot{}, errors.New("contract symbol is required for market data")
		}
		return quote(ctx, c, d)
	}
	res, err := rig.preview(rpc.OrderActionBuy, cusip, "BOND", "USD", 1000)
	if err != nil || quoted.Symbol != cusip || res.Draft.Contract.Symbol != cusip {
		t.Fatalf("preview of a line without symbol: %v; quoted %+v", err, quoted)
	}
}

// The order a bond preview signs goes to IBKR by contract id alone: the
// quote may carry the CUSIP as its symbol, the order may not (IBKR error 478).
func TestBondOrderContractGoesByContractIDAlone(t *testing.T) {
	c := previewIBKRContract(rpc.ContractParams{ConID: 928489461, Symbol: "91282CRM5", SecType: "BOND", Exchange: "SMART", Currency: "USD"})
	if c.Symbol != "" || c.ConID != 928489461 || c.SecType != "BOND" {
		t.Fatalf("bond order contract = %+v, want contract id only", c)
	}
	if s := previewIBKRContract(rpc.ContractParams{Symbol: "AAA", SecType: "STK", Currency: "USD"}); s.Symbol != "AAA" {
		t.Fatalf("a stock lost its symbol: %+v", s)
	}
}

// The quote of a bill or bond keeps its CUSIP or ISIN as the symbol: Canary's
// market data keys a subscription by symbol, and IBKR accepted the CUSIP with
// the contract id (live 2026-10-07). Only the order goes by contract id alone;
// dropping the symbol from the quote too refused every bond preview ("contract
// symbol is required for market data", live 13:26 CEST on 3644bf05).
func TestBondQuoteContractKeepsItsSymbol(t *testing.T) {
	bond := rpc.ContractParams{ConID: 928489461, Symbol: "91282CRM5", SecType: "BOND", Exchange: "SMART", Currency: "USD"}
	if q := previewIBKRQuoteContract(bond); q.Symbol != "91282CRM5" || q.ConID != 928489461 || q.SecType != "BOND" || q.Exchange != "SMART" || q.Currency != "USD" {
		t.Fatalf("bond quote contract = %+v, want the CUSIP symbol with the contract id", q)
	}
	if o := previewIBKRContract(bond); o.Symbol != "" {
		t.Fatalf("bond order contract = %+v, want no symbol", o)
	}
	stock := rpc.ContractParams{Symbol: "AAA", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	if q, o := previewIBKRQuoteContract(stock), previewIBKRContract(stock); q.Symbol != o.Symbol || q.ConID != o.ConID || q.SecType != o.SecType || q.Exchange != o.Exchange || q.Currency != o.Currency || q.PrimaryExch != o.PrimaryExch {
		t.Fatalf("a stock quote contract %+v differs from its order contract %+v", q, o)
	}
}

// TWS's negative yield-to-worst confirmation is bypassed for API orders
// (owner decision 2026-10-07 19:39 CEST); Canary refuses a buy priced at or
// above what the bond still pays.
func TestBondBuyYieldRefusal(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	terms := func(maturity string, coupon *float64) *rpc.OrderBondTerms {
		return &rpc.OrderBondTerms{Maturity: maturity, Coupon: coupon}
	}
	cases := []struct {
		name   string
		price  float64
		terms  *rpc.OrderBondTerms
		refuse bool
	}{
		{"a 7-year 5% note below par", 99.14, terms("2033-09-30", new(5.0)), false},
		{"a premium bond that still yields", 103, terms("2030-10-07", new(2.0)), false}, // pays 108
		{"a premium bond at what it pays", 108.1, terms("2030-10-07", new(2.0)), true},
		{"a bill below par", 99.2, terms("2027-01-05", nil), false},
		{"a bill at par", 100, terms("2027-01-05", nil), true},
		{"a zero above par", 100.4, terms("2029-02-15", new(0.0)), true},
		{"no maturity", 99, terms("", new(3.0)), true},
	}
	for _, c := range cases {
		err := bondBuyYieldRefusal(c.price, c.terms, now)
		if (err != nil) != c.refuse {
			t.Errorf("%s: err = %v, refuse %v", c.name, err, c.refuse)
		}
		if err != nil && !slices.Contains(refusalCodes(err), previewBondNegativeYieldCode) {
			t.Errorf("%s: codes %v", c.name, refusalCodes(err))
		}
	}
	// End to end: a zero-coupon Bund quoted above par is refused before WhatIf.
	rig := newBondRequestRig(t)
	bund := syntheticISIN(t, "DE000SYN000")
	rig.line(bund, "EUR", 8901)
	rig.ecb[bund] = ecbEligibleAsset{ISIN: bund, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD1", IssuerGroup: "IG2", Maturity: rig.now.AddDate(3, 0, 0)}
	rig.srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 100.20, 100.24
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	if _, err := rig.preview(rpc.OrderActionBuy, bund, "BOND", "EUR", 10000); err == nil || !slices.Contains(refusalCodes(err), previewBondNegativeYieldCode) {
		t.Fatalf("a negative-yield buy: %v %v", err, refusalCodes(err))
	}
	if len(rig.drafts) != 0 {
		t.Fatal("a negative-yield buy reached the broker's WhatIf")
	}
	// A sale is never refused for yield.
	if _, err := rig.preview(rpc.OrderActionSell, bund, "BOND", "EUR", 10000); err != nil {
		t.Fatalf("a sale above par: %v %v", err, refusalCodes(err))
	}
}
