package daemon

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Synthetic bill identifiers with valid check digits; none names a real
// security.
const (
	synthCUSIP35  = "912797ZZ3"
	synthCUSIP60  = "912797ZY6"
	synthCUSIP90  = "912797ZX8"
	synthCUSIPOld = "912797ZW0"
	synthDEBill   = "DE000BU0ZZ19"
	synthDEBill2  = "DE000BU0ZZ27"
	synthFRBill   = "FR0128ZZZZ13"
	synthGBBill   = "GB00ZZZZZZ11"
)

// synthBondLine is a zero-coupon line issued 91 days before it matures.
func synthBondLine(conID int, id, ccy string, maturity time.Time) ibkrlib.BondContractDetails {
	line := ibkrlib.BondContractDetails{ConID: conID, Symbol: "SYNTHB", SecType: "BOND", CUSIPField: id, Currency: ccy,
		Maturity: maturity.Format("20060102"), IssueDate: maturity.AddDate(0, 0, -91).Format("20060102"), Exchange: "SMART",
		MinTick: 0.0001, MinSize: 1000, SizeIncrement: 1000, Complete: true, SecIDs: map[string]string{}}
	if ibkrlib.ValidISIN(id) {
		line.SecIDs["ISIN"] = id
	} else {
		line.SecIDs["CUSIP"] = id
	}
	return line
}

func synthLiveQuote(ask float64) rpc.BondQuote {
	bid := ask - 0.02
	return rpc.BondQuote{Bid: &bid, Ask: &ask, BidYield: new(4.1), DataType: "live", Fresh: true, PriceConvention: rpc.BondPriceConventionPer100}
}

// fakeBillSource answers from maps keyed by identifier and contract id.
type fakeBillSource struct {
	bills    []treasuryBill
	at       time.Time
	reason   string
	byID     map[string][]ibkrlib.BondContractDetails
	lineErr  map[string]error
	quotes   map[int]rpc.BondQuote
	quoteErr map[int]error
	asked    []string
}

func (f *fakeBillSource) usBills(time.Time) ([]treasuryBill, time.Time, string) {
	return f.bills, f.at, f.reason
}

func (f *fakeBillSource) lines(_ context.Context, idType, id, ccy string) ([]ibkrlib.BondContractDetails, error) {
	f.asked = append(f.asked, idType+":"+id+":"+ccy)
	if err := f.lineErr[id]; err != nil {
		return nil, err
	}
	return f.byID[id], nil
}

func (f *fakeBillSource) quote(_ context.Context, line ibkrlib.BondContractDetails) (rpc.BondQuote, error) {
	if err := f.quoteErr[line.ConID]; err != nil {
		return rpc.BondQuote{}, err
	}
	q, ok := f.quotes[line.ConID]
	if !ok {
		return rpc.BondQuote{StaleReason: "no price arrived"}, nil
	}
	return q, nil
}

// usBillSource lists four issued bills (20, 35, 60 and 90 days) and one
// announced but not yet issued, each resolvable and quoted live.
func usBillSource(now time.Time) *fakeBillSource {
	day := cashSweepDay(now)
	bill := func(cusip string, days, issuedDaysAgo int) treasuryBill {
		return treasuryBill{CUSIP: cusip, IssueDate: day.AddDate(0, 0, -issuedDaysAgo).Format(time.DateOnly), MaturityDate: day.AddDate(0, 0, days).Format(time.DateOnly)}
	}
	src := &fakeBillSource{at: now.Add(-time.Hour), byID: map[string][]ibkrlib.BondContractDetails{}, quotes: map[int]rpc.BondQuote{}}
	src.bills = []treasuryBill{bill(synthCUSIPOld, 20, 10), bill(synthCUSIP35, 35, 20), bill(synthCUSIP60, 60, 30), bill(synthCUSIP90, 90, 1), bill("912797ZV2", 30, -7)}
	for i, b := range src.bills {
		maturity, _ := time.Parse(time.DateOnly, b.MaturityDate)
		conID := 7100 + i
		src.byID[b.CUSIP] = []ibkrlib.BondContractDetails{synthBondLine(conID, b.CUSIP, "USD", maturity)}
		src.quotes[conID] = synthLiveQuote(99.5 + float64(i)/10)
	}
	return src
}

func planAndResolve(t *testing.T, policy protectionPolicy, in cashSweepInput, src cashSweepBillSource) (cashSweepPlan, map[string]cashSweepCurrencyPlan) {
	t.Helper()
	now := cashSweepTestNow()
	plan := cashSweepPlanFor(policy, in, now)
	cashSweepResolveBills(context.Background(), src, policy.Buckets.CashSweep, &plan, now)
	byCcy := map[string]cashSweepCurrencyPlan{}
	for i, cp := range plan.currencies {
		if st := plan.status.Currencies[i]; st.Currency != cp.status.Currency || st.State != cp.status.State || st.Reason != cp.status.Reason ||
			!slices.Equal(st.Evidence, cp.status.Evidence) || (st.Bill == nil) != (cp.status.Bill == nil) {
			t.Fatalf("status and plan diverged for %s", cp.status.Currency)
		}
		byCcy[cp.status.Currency] = cp
	}
	return plan, byCcy
}

// Settled cash comes from the broker's ledger field when it is there; the
// journal is the fallback; settlement_unknown only when both are missing.
func TestCashSweepSettledCashFromBroker(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	withBroker := func(settled float64) cashSweepInput {
		in := cashSweepTestInput(map[string]float64{"USD": 60000})
		row := in.Ledger["USD"]
		row.Settled = new(settled)
		in.Ledger["USD"] = row
		return in
	}
	// Journal gap, broker tag present: the broker's figure decides and every
	// unsettled net proceed counts toward keep_cash on the redeem side.
	in := withBroker(52000)
	in.Settlement = cashSweepSettlement{Reason: "the daemon started inside the window"}
	st := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD").status
	if st.State != rpc.CashSweepStateInvest || st.SettledCashSource != rpc.CashSweepSettledSourceBroker || *st.SettledCash != 52000 || *st.Cash != 52000 ||
		*st.PendingRedemptions != 8000 || *st.Free != 47000 {
		t.Fatalf("broker settled, journal gap = %+v", st)
	}
	// Journal known too: pending redemptions are the journal's equivalent sales.
	in = withBroker(52000)
	in.Settlement.EquivalentSales["USD"] = 300
	in.Settlement.SaleProceeds["USD"] = 9999 // ignored: the broker's settled cash wins
	st = cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD").status
	if st.SettledCashSource != rpc.CashSweepSettledSourceBroker || *st.SettledCash != 52000 || *st.PendingRedemptions != 300 {
		t.Fatalf("broker settled, journal known = %+v", st)
	}
	// No broker tag: the journal derivation, labelled as such.
	in = cashSweepTestInput(map[string]float64{"USD": 60000})
	in.Settlement.SaleProceeds["USD"] = 8000
	st = cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD").status
	if st.SettledCashSource != rpc.CashSweepSettledSourceJournal || *st.SettledCash != 52000 {
		t.Fatalf("journal settled = %+v", st)
	}
	// Neither: settlement_unknown, naming the missing ledger field.
	in.Settlement = cashSweepSettlement{Reason: "no order journal is attached"}
	st = cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD").status
	if st.State != rpc.CashSweepStateSettlementUnknown || !strings.Contains(st.Reason, "SettledCash") || st.SettledCashSource != "" || st.Cash != nil {
		t.Fatalf("both unknown = %+v", st)
	}
	// A non-finite broker figure is not an observation.
	in = withBroker(0)
	*in.Ledger["USD"].Settled = math.NaN()
	in.Settlement = cashSweepSettlement{Reason: "gap"}
	if st := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD").status; st.State != rpc.CashSweepStateSettlementUnknown {
		t.Fatalf("NaN settled = %+v", st)
	}
}

// cash_like is cash plus cash equivalents, present only when both are.
func TestCashSweepStatusCashLike(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := cashSweepTestInput(map[string]float64{"USD": 20000, "EUR": 20000})
	in.Holdings["USD"] = []cashSweepHolding{cashSweepTestBill(801, "USD", "us_tbill", 10, 60)}
	in.Unclassified["EUR"] = "1 bond"
	plan := cashSweepPlanFor(policy, in, now)
	usd, eur := cashSweepCurrencyOf(t, plan, "USD").status, cashSweepCurrencyOf(t, plan, "EUR").status
	if usd.CashLike == nil || *usd.CashLike != 20000+9900 {
		t.Fatalf("USD cash_like = %v", usd.CashLike)
	}
	if eur.CashLike != nil || eur.Cash == nil {
		t.Fatalf("EUR cash_like without equivalents = %v", eur.CashLike)
	}
}

// A held BOND row is a cash equivalent when the bonds section classifies it
// as a vocabulary bill of its own currency; a coupon bond or another
// country's bill is not; an unresolved row makes its currency unclassified.
func TestCashSweepClassifiesHeldBills(t *testing.T) {
	now := cashSweepTestNow()
	day := cashSweepDay(now)
	bucket := &protectionCashSweepPolicy{Enabled: true}
	pos := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "SYNTHB", SecType: "BOND", ConID: 901, Currency: "USD", Quantity: 5, MarketValue: 4950},
			{Symbol: "SYNTHN", SecType: "BOND", ConID: 902, Currency: "USD", Quantity: 3, MarketValue: 3000},
			{Symbol: "SYNTHD", SecType: "BOND", ConID: 903, Currency: "EUR", Quantity: 20000, MarketValue: 19800},
			{Symbol: "SYNTHG", SecType: "BOND", ConID: 905, Currency: "EUR", Quantity: 1000, MarketValue: 990},
			{Symbol: "SYNTHU", SecType: "BOND", ConID: 904, Currency: "CAD", Quantity: 1000, MarketValue: 990},
		},
		Bonds: []rpc.PositionBond{
			{ConID: 901, Currency: "USD", Class: rpc.BondClassBill, CUSIP: synthCUSIP35, Maturity: day.AddDate(0, 0, 35).Format(time.DateOnly)},
			{ConID: 902, Currency: "USD", Class: rpc.BondClassBond, CUSIP: "91282CZZ7", Maturity: day.AddDate(0, 0, 200).Format(time.DateOnly)},
			{ConID: 903, Currency: "EUR", Class: rpc.BondClassBill, ISIN: synthDEBill, Maturity: day.AddDate(0, 0, 100).Format(time.DateOnly)},
			{ConID: 905, Currency: "EUR", Class: rpc.BondClassBill, ISIN: synthGBBill, Maturity: day.AddDate(0, 0, 50).Format(time.DateOnly)},
			{ConID: 904, Currency: "CAD", Class: rpc.BondClassUnresolved, Reason: "contract details unavailable"},
		},
	}
	holdings, unclassified := cashSweepClassify(bucket, pos)
	if len(holdings["USD"]) != 1 || holdings["USD"][0].Instrument != cashSweepInstrumentUSTBill || holdings["USD"][0].FaceValue != 5000 ||
		cashSweepDaysLeft(day, holdings["USD"][0].Maturity) != 35 {
		t.Fatalf("USD holdings = %+v", holdings["USD"])
	}
	if len(holdings["EUR"]) != 1 || holdings["EUR"][0].Instrument != cashSweepInstrumentDEBubill || holdings["EUR"][0].FaceValue != 20000 {
		t.Fatalf("EUR holdings = %+v", holdings["EUR"])
	}
	if len(unclassified) != 1 || unclassified["CAD"] == "" {
		t.Fatalf("unclassified = %v", unclassified)
	}
	// The classified bills feed the rungs and the redemption; the unresolved
	// CAD row stops only CAD.
	in := cashSweepTestInput(map[string]float64{"USD": 3000, "EUR": 20000, "CAD": 20000})
	in.Holdings, in.Unclassified = holdings, unclassified
	plan := cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9), in, now)
	if usd := cashSweepCurrencyOf(t, plan, "USD"); usd.side != rpc.CashSweepSideRedeem || usd.holding.Row.ConID != 901 {
		t.Fatalf("USD = %+v", usd.status)
	}
	if cad := cashSweepCurrencyOf(t, plan, "CAD"); cad.status.State != rpc.CashSweepStateEquivalentsUnclassified {
		t.Fatalf("CAD = %+v", cad.status)
	}
}

// USD: the issued bill maturing nearest the rung's target inside the window
// is resolved by CUSIP and quoted; the row names it.
func TestCashSweepResolvesNearestUSBill(t *testing.T) {
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	src := usBillSource(cashSweepTestNow())
	_, got := planAndResolve(t, policy, cashSweepTestInput(map[string]float64{"USD": 60000}), src)
	usd := got["USD"]
	// Rung 1 targets 28 days: the 20-day bill is under the window and the
	// unissued one is skipped, so the 35-day bill is nearest.
	if usd.side != rpc.CashSweepSideInvest || usd.bill == nil || usd.bill.CUSIP != synthCUSIP35 || usd.bill.DaysToMaturity != 35 ||
		usd.bill.Source != rpc.CashSweepBillSourceTreasuryDirect || usd.bill.PriceSource != "ask" || *usd.bill.Price != 99.6 || !usd.bill.QuoteFresh ||
		usd.bill.QuantityUnit != rpc.BondQuantityUnitFace1000 || usd.bill.PriceConvention != rpc.BondPriceConventionPer100 {
		t.Fatalf("USD bill = %+v (%s)", usd.bill, usd.status.Reason)
	}
	if usd.status.Bill == nil || usd.status.Bill.CUSIP != synthCUSIP35 || !strings.Contains(usd.status.Reason, "CUSIP "+synthCUSIP35) {
		t.Fatalf("status = %+v", usd.status)
	}
	if !slices.Equal(src.asked, []string{"CUSIP:" + synthCUSIP35 + ":USD"}) {
		t.Fatalf("asked = %v", src.asked)
	}
	now := cashSweepTestNow()
	plan := cashSweepPlan{status: rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeShadow, Shadow: true}}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, usd)
	if row.Contract.ConID != usd.bill.ConID || row.Contract.SecType != "BOND" || row.Contract.Currency != "USD" || row.CashSweep.Bill == nil ||
		row.CashSweep.Bill.CUSIP != synthCUSIP35 || row.CashSweep.Instrument != cashSweepInstrumentUSTBill {
		t.Fatalf("row = %+v / %+v", row.Contract, row.CashSweep)
	}
	var codes []string
	for _, b := range row.Blockers {
		codes = append(codes, b.Code)
	}
	if !slices.Equal(codes, []string{"shadow_mode", rpc.CashSweepBlockerInstrumentSupport}) {
		t.Fatalf("codes = %v", codes)
	}
	if _, ok := cashSweepOpenException(row); !ok {
		t.Fatalf("a resolved bill left the typed exception: %+v", row.CashSweep)
	}
	if !slices.ContainsFunc(row.Details, func(d string) bool {
		return strings.Contains(d, "CUSIP "+synthCUSIP35) && strings.Contains(d, "99.6000")
	}) {
		t.Fatalf("details = %v", row.Details)
	}
	// A cloned snapshot does not share the bill.
	copied := rpc.CloneProposalCashSweep(row.CashSweep)
	*copied.Bill.Price = -1
	if *row.CashSweep.Bill.Price == -1 {
		t.Fatal("the row's bill is shared by its clone")
	}
}

// A candidate whose broker maturity differs from the list, or whose quote
// carries no price, is skipped with evidence; a stale quote keeps the row
// and adds fresh_bill_quote_required; nothing confirmed reads
// instrument_unresolved; no list reads universe_unavailable.
func TestCashSweepBillResolutionFailsClosed(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := func() cashSweepInput { return cashSweepTestInput(map[string]float64{"USD": 60000}) }

	src := usBillSource(now)
	src.byID[synthCUSIP35][0].Maturity = cashSweepDay(now).AddDate(0, 0, 36).Format("20060102")
	_, got := planAndResolve(t, policy, in(), src)
	if b := got["USD"].bill; b == nil || b.CUSIP != synthCUSIP60 {
		t.Fatalf("maturity mismatch did not move to the next bill: %+v", got["USD"].status)
	}

	src = usBillSource(now)
	stale := synthLiveQuote(99.6)
	stale.Fresh, stale.DataType, stale.StaleReason = false, "frozen", "the bid or ask is frozen, not live"
	src.quotes[7101] = stale
	_, got = planAndResolve(t, policy, in(), src)
	usd := got["USD"]
	if usd.bill == nil || usd.bill.QuoteFresh {
		t.Fatalf("stale quote = %+v", usd.status)
	}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, cashSweepPlan{status: rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeActive}}, usd)
	if !slices.ContainsFunc(row.Blockers, func(b rpc.TradingBlocker) bool { return b.Code == rpc.CashSweepBlockerFreshQuote }) {
		t.Fatalf("stale quote row blockers = %+v", row.Blockers)
	}

	src = usBillSource(now)
	for id := range src.quotes {
		delete(src.quotes, id)
	}
	src.lineErr = map[string]error{synthCUSIP60: ibkrlib.ErrContractNoDefinition}
	_, got = planAndResolve(t, policy, in(), src)
	usd = got["USD"]
	if usd.side != "" || usd.bill != nil || usd.status.State != rpc.CashSweepStateInstrumentUnresolved || len(usd.status.Evidence) != 3 ||
		!strings.Contains(strings.Join(usd.status.Evidence, "\n"), "IBKR lists no such bond line") || usd.status.Free == nil {
		t.Fatalf("unresolved = %+v", usd.status)
	}

	src = usBillSource(now)
	src.reason = "TreasuryDirect's bill list is unreachable: HTTP 503"
	_, got = planAndResolve(t, policy, in(), src)
	if usd := got["USD"]; usd.side != "" || usd.status.State != rpc.CashSweepStateUniverseUnavailable || !strings.Contains(usd.status.Reason, "HTTP 503") {
		t.Fatalf("universe unavailable = %+v", usd.status)
	}

	// No source at all (no gateway): unresolved, never a row.
	_, got = planAndResolve(t, policy, in(), nil)
	if usd := got["USD"]; usd.side != "" || usd.status.State != rpc.CashSweepStateInstrumentUnresolved {
		t.Fatalf("no source = %+v", usd.status)
	}
}

// EUR: the owner's listed ISINs are the universe; empty reads
// universe_unavailable; a listed bill outside the window is evidence; the
// nearest confirmed bill of a declared instrument is named.
func TestCashSweepResolvesListedEURBills(t *testing.T) {
	now := cashSweepTestNow()
	day := cashSweepDay(now)
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := func() cashSweepInput { return cashSweepTestInput(map[string]float64{"EUR": 60000}) }

	_, got := planAndResolve(t, policy, in(), &fakeBillSource{})
	if eur := got["EUR"]; eur.status.State != rpc.CashSweepStateUniverseUnavailable || !strings.Contains(eur.status.Reason, "isins") {
		t.Fatalf("no isins = %+v", eur.status)
	}

	setSweepCcy(policy.Buckets.CashSweep, "EUR", func(c *protectionCashSweepCurrency) {
		c.ISINs = []string{synthDEBill2, synthFRBill, synthDEBill}
	})
	src := &fakeBillSource{byID: map[string][]ibkrlib.BondContractDetails{
		synthDEBill2: {synthBondLine(7201, synthDEBill2, "EUR", day.AddDate(0, 0, 300))},
		synthFRBill:  {synthBondLine(7202, synthFRBill, "EUR", day.AddDate(0, 0, 60))},
		synthDEBill:  {synthBondLine(7203, synthDEBill, "EUR", day.AddDate(0, 0, 100))},
	}, quotes: map[int]rpc.BondQuote{7202: synthLiveQuote(99.7), 7203: synthLiveQuote(99.4)}}
	_, got = planAndResolve(t, policy, in(), src)
	eur := got["EUR"]
	// Rung 1 targets 28 days: the French 60-day bill is nearest; the 300-day
	// German bill is outside 28–182.
	if eur.bill == nil || eur.bill.ISIN != synthFRBill || eur.bill.Instrument != cashSweepInstrumentFRBTF || eur.bill.Source != rpc.CashSweepBillSourcePolicyISINs ||
		eur.bill.QuantityUnit != rpc.BondQuantityUnitFace1 {
		t.Fatalf("EUR bill = %+v (%s)", eur.bill, eur.status.Reason)
	}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, cashSweepPlan{status: rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeShadow, Shadow: true}}, eur)
	if row.CashSweep.Instrument != cashSweepInstrumentFRBTF || row.Key != cashSweepKey("EUR", rpc.CashSweepSideInvest, cashSweepInstrumentDEBubill, 0) {
		t.Fatalf("row instrument %s key %s", row.CashSweep.Instrument, row.Key)
	}
	if _, ok := cashSweepOpenException(row); !ok {
		t.Fatal("a French bill for EUR left the typed exception")
	}

	// Every listed bill outside the window or unresolvable: evidence per ISIN.
	src.byID[synthFRBill] = []ibkrlib.BondContractDetails{synthBondLine(7202, synthFRBill, "EUR", day.AddDate(0, 0, 10))}
	src.lineErr = map[string]error{synthDEBill: errBondLookupPending}
	_, got = planAndResolve(t, policy, in(), src)
	eur = got["EUR"]
	if eur.status.State != rpc.CashSweepStateInstrumentUnresolved || len(eur.status.Evidence) != 3 ||
		!strings.Contains(strings.Join(eur.status.Evidence, "\n"), "outside 28–182 days") || !strings.Contains(strings.Join(eur.status.Evidence, "\n"), "still running") {
		t.Fatalf("EUR unresolved = %+v", eur.status)
	}
}

// isins are validated: a check digit, an instrument the currency declares,
// once each, and never for USD.
func TestCashSweepISINValidation(t *testing.T) {
	p, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + `
[buckets.cash_sweep]
enabled = true

[buckets.cash_sweep.currency.EUR]
isins = ["` + synthDEBill + `", "` + synthFRBill + `"]
`))
	if err != nil || !slices.Equal(p.Buckets.CashSweep.currency("EUR").ISINs, []string{synthDEBill, synthFRBill}) {
		t.Fatalf("isins = %+v err %v", p.Buckets.CashSweep, err)
	}
	for name, tc := range map[string]struct{ table, want string }{
		"bad check digit":   {"[buckets.cash_sweep.currency.EUR]\nisins = [\"DE000BU0ZZ18\"]\n", "not an ISIN"},
		"another currency":  {"[buckets.cash_sweep.currency.EUR]\nisins = [\"" + synthGBBill + "\"]\n", "not a bill of an instrument declared for EUR"},
		"undeclared issuer": {"[buckets.cash_sweep.currency.EUR]\ninstruments = [\"de_bubill\"]\nfallback = \"none\"\nisins = [\"" + synthFRBill + "\"]\n", "declared for EUR"},
		"duplicate":         {"[buckets.cash_sweep.currency.EUR]\nisins = [\"" + synthDEBill + "\", \"" + synthDEBill + "\"]\n", "twice"},
		"usd":               {"[buckets.cash_sweep.currency.USD]\nisins = [\"US912797ZZ37\"]\n", "TreasuryDirect"},
	} {
		if _, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[buckets.cash_sweep]\nenabled = true\n" + tc.table)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// An absent list keeps an existing file's fingerprint.
	without, _, _ := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[buckets.cash_sweep.currency.EUR]\nkeep_cash = 8000\n"))
	empty, _, _ := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[buckets.cash_sweep.currency.EUR]\nkeep_cash = 8000\nisins = []\n"))
	if fingerprintProtectionPolicy(without).Key != fingerprintProtectionPolicy(empty).Key {
		t.Fatal("an empty isins list changed the fingerprint")
	}
}

// TreasuryDirect's list: bills with a CUSIP, issue and maturity date; a
// reopened CUSIP counts once; an announced record without a maturity and a
// note are skipped.
func TestParseTreasuryDirectBills(t *testing.T) {
	body := `[
{"cusip":"912797ZZ3","issueDate":"2026-09-03T00:00:00","maturityDate":"2026-11-05T00:00:00","securityType":"Bill","securityTerm":"8-Week"},
{"cusip":"912797ZZ3","issueDate":"2026-10-01T00:00:00","maturityDate":"2026-11-05T00:00:00","securityType":"Bill","securityTerm":"5-Week"},
{"cusip":"912797ZY6","issueDate":"2026-10-08T00:00:00","maturityDate":"","securityType":"Bill","securityTerm":"6-Week"},
{"cusip":"91282CZZ7","issueDate":"2026-09-30T00:00:00","maturityDate":"2028-09-30T00:00:00","securityType":"Note","securityTerm":"2-Year"},
{"cusip":"912797ZX9","issueDate":"2026-09-10T00:00:00","maturityDate":"2027-03-11T00:00:00","securityType":"Bill","securityTerm":"26-Week"},
{"cusip":"912797ZX8","issueDate":"2026-09-10T00:00:00","maturityDate":"2027-03-11T00:00:00","securityType":"Bill","securityTerm":"26-Week"}
]`
	bills, err := parseTreasuryDirectBills(strings.NewReader(body))
	if err != nil || len(bills) != 2 || bills[0].CUSIP != synthCUSIP35 || bills[0].IssueDate != "2026-09-03" || bills[0].MaturityDate != "2026-11-05" ||
		bills[1].CUSIP != synthCUSIP90 || bills[1].MaturityDate != "2027-03-11" {
		t.Fatalf("bills = %+v err %v", bills, err)
	}
	if _, err := parseTreasuryDirectBills(strings.NewReader(`[]`)); err == nil {
		t.Fatal("an empty list parsed")
	}
	if _, err := parseTreasuryDirectBills(strings.NewReader(`{"error":"x"}`)); err == nil {
		t.Fatal("an object parsed")
	}
}

// The list refreshes once a day, serves up to two days, retries a failure
// after 15 minutes, and a failure with nothing to serve reads unreachable.
func TestTreasuryBillUniverseAging(t *testing.T) {
	now := cashSweepTestNow()
	fetches := 0
	fail := errors.New("HTTP 503")
	var next error
	u := &billUniverse{fetch: func(context.Context) ([]treasuryBill, error) {
		fetches++
		if next != nil {
			return nil, next
		}
		return []treasuryBill{{CUSIP: synthCUSIP35, IssueDate: "2026-09-03", MaturityDate: "2026-11-05"}}, nil
	}}
	if _, _, reason := u.snapshot(now); !strings.Contains(reason, "not been read") || !u.due(now) {
		t.Fatalf("cold universe = %q due %v", reason, u.due(now))
	}
	next = fail
	if err := u.refresh(context.Background(), now, nil); err == nil {
		t.Fatal("a failed read succeeded")
	}
	if _, _, reason := u.snapshot(now); !strings.Contains(reason, "unreachable: HTTP 503") || u.due(now.Add(10*time.Minute)) || !u.due(now.Add(15*time.Minute)) {
		t.Fatalf("after failure = %q", reason)
	}
	next = nil
	var persisted treasuryBillUniverseRecord
	if err := u.refresh(context.Background(), now, func(_ context.Context, rec treasuryBillUniverseRecord) error { persisted = rec; return nil }); err != nil {
		t.Fatal(err)
	}
	if bills, at, reason := u.snapshot(now.Add(47 * time.Hour)); reason != "" || len(bills) != 1 || !at.Equal(now) || len(persisted.Bills) != 1 {
		t.Fatalf("served = %v %v %q persisted %+v", bills, at, reason, persisted)
	}
	if u.due(now.Add(23*time.Hour)) || !u.due(now.Add(25*time.Hour)) {
		t.Fatal("refresh cadence is not daily")
	}
	if _, _, reason := u.snapshot(now.Add(49 * time.Hour)); !strings.Contains(reason, "more than two days ago") {
		t.Fatalf("stale list = %q", reason)
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d", fetches)
	}
}

// The directory asks the gateway once per request per day, retries a
// failure after ten minutes, and lets a slow lookup finish detached.
func TestBondDirectoryCachesAndDetaches(t *testing.T) {
	now := cashSweepTestNow()
	var calls atomic.Int32
	release := make(chan struct{})
	dir := &bondDirectory{now: func() time.Time { return now }}
	dir.fetch = func(_ context.Context, r ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, error) {
		calls.Add(1)
		if r.ID == synthCUSIP60 {
			<-release
		}
		if r.ID == synthCUSIP90 {
			return nil, ibkrlib.ErrContractNoDefinition
		}
		return []ibkrlib.BondContractDetails{synthBondLine(7300, r.ID, r.Currency, now.AddDate(0, 0, 35))}, nil
	}
	req := ibkrlib.BondContractRequest{IDType: "CUSIP", ID: synthCUSIP35, Currency: "USD"}
	for range 3 {
		if lines, err := dir.lookup(context.Background(), req, time.Second); err != nil || len(lines) != 1 {
			t.Fatalf("lookup = %v %v", lines, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	failing := ibkrlib.BondContractRequest{IDType: "CUSIP", ID: synthCUSIP90, Currency: "USD"}
	_, _ = dir.lookup(context.Background(), failing, time.Second)
	_, _ = dir.lookup(context.Background(), failing, time.Second)
	if calls.Load() != 2 {
		t.Fatalf("a failure was retried at once: %d", calls.Load())
	}
	now = now.Add(11 * time.Minute)
	if _, err := dir.lookup(context.Background(), failing, time.Second); !errors.Is(err, ibkrlib.ErrContractNoDefinition) || calls.Load() != 3 {
		t.Fatalf("retry after ten minutes: %v calls %d", err, calls.Load())
	}
	// A transient failure (no gateway) is retried after thirty seconds.
	if bondLinesTTL(ibkrlib.ErrIBKRUnavailable) != bondDetailsTransientRetry || bondLinesTTL(context.DeadlineExceeded) != bondDetailsTransientRetry {
		t.Fatal("a transient failure is cached like a missing line")
	}
	slow := ibkrlib.BondContractRequest{IDType: "CUSIP", ID: synthCUSIP60, Currency: "USD"}
	if _, err := dir.lookup(context.Background(), slow, 10*time.Millisecond); !errors.Is(err, errBondLookupPending) {
		t.Fatalf("slow lookup = %v", err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		if lines, err := dir.lookup(context.Background(), slow, 10*time.Millisecond); err == nil && len(lines) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached lookup never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 4 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

// Held BOND rows are classified bill or bond by their contract details;
// a row the gateway cannot describe is unresolved and says why.
func TestClassifyBondPositions(t *testing.T) {
	now := cashSweepTestNow()
	day := cashSweepDay(now)
	coupon := synthBondLine(7402, "91282CZZ7", "USD", day.AddDate(0, 0, 200))
	coupon.Coupon = 4.25
	coupon.IssueDate = day.AddDate(-2, 0, 0).Format("20060102")
	lines := map[int]ibkrlib.BondContractDetails{
		7401: synthBondLine(7401, synthCUSIP35, "USD", day.AddDate(0, 0, 35)),
		7402: coupon,
		7403: synthBondLine(7403, synthDEBill, "EUR", day.AddDate(0, 0, 100)),
	}
	s := &Server{}
	s.cashSweepB.dir = &bondDirectory{fetch: func(_ context.Context, r ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, error) {
		if line, ok := lines[r.ConID]; ok {
			return []ibkrlib.BondContractDetails{line}, nil
		}
		return nil, ibkrlib.ErrBondContractNotFound
	}}
	rows := []rpc.PositionView{
		{Symbol: "SYNTH", SecType: "STK", ConID: 1, Currency: "USD", Quantity: 10},
		{Symbol: "SYNTHB", SecType: "BOND", ConID: 7401, Currency: "USD", Quantity: 5, Mark: 99.5, MarketValue: 4975},
		{Symbol: "SYNTHN", SecType: "BOND", ConID: 7402, Currency: "USD", Quantity: 3, MarketValue: 3000},
		{Symbol: "SYNTHD", SecType: "BOND", ConID: 7403, Currency: "EUR", Quantity: 20000, MarketValue: 19800},
		{Symbol: "SYNTHX", SecType: "BOND", ConID: 7404, Currency: "CAD", Quantity: 1000, MarketValue: 990},
		{Symbol: "SYNTHY", SecType: "BOND", Currency: "CAD", Quantity: 1000},
	}
	got := s.classifyBondPositions(context.Background(), rows, now)
	if len(got) != 5 {
		t.Fatalf("bonds = %+v", got)
	}
	byCon := map[int]rpc.PositionBond{}
	for _, b := range got {
		byCon[b.ConID] = b
	}
	if b := byCon[7401]; b.Class != rpc.BondClassBill || b.CUSIP != synthCUSIP35 || b.Maturity != day.AddDate(0, 0, 35).Format(time.DateOnly) || *b.DaysToMaturity != 35 || b.MarketValue != 4975 {
		t.Fatalf("US bill = %+v", b)
	}
	if b := byCon[7402]; b.Class != rpc.BondClassBond || *b.Coupon != 4.25 {
		t.Fatalf("coupon bond = %+v", b)
	}
	if b := byCon[7403]; b.Class != rpc.BondClassBill || b.ISIN != synthDEBill || b.Currency != "EUR" {
		t.Fatalf("DE bill = %+v", b)
	}
	if b := byCon[7404]; b.Class != rpc.BondClassUnresolved || !strings.Contains(b.Reason, "no such bond line") {
		t.Fatalf("unknown = %+v", b)
	}
	if b := byCon[0]; b.Class != rpc.BondClassUnresolved || !strings.Contains(b.Reason, "contract id") {
		t.Fatalf("no conid = %+v", b)
	}
	if s.classifyBondPositions(context.Background(), rows[:1], now) != nil {
		t.Fatal("a book without bonds grew a bonds section")
	}
}

// canary market --type BOND: identifier and currency, then one line and
// one quote; every gap is data, not an error.
func TestMarketBondCheck(t *testing.T) {
	for _, tc := range []struct{ in, ccy, idType, wantCcy, err string }{
		{synthCUSIP35, "", "CUSIP", "USD", ""},
		{strings.ToLower(synthDEBill), "", "ISIN", "EUR", ""},
		{synthGBBill, "", "ISIN", "GBP", ""},
		{"XS0000000009", "", "", "", "give --currency"},
		{"XS0000000009", "eur", "ISIN", "EUR", ""},
		{"DE000BU0ZZ18", "", "", "", "neither an ISIN"},
	} {
		idType, _, ccy, err := bondIdentifierFor(tc.in, tc.ccy)
		if (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) || idType != tc.idType || ccy != tc.wantCcy {
			t.Fatalf("%s/%s = %s %s %v", tc.in, tc.ccy, idType, ccy, err)
		}
	}
	now := cashSweepTestNow()
	day := cashSweepDay(now)
	dir := &bondDirectory{
		fetch: func(_ context.Context, r ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, error) {
			switch r.ID {
			case synthDEBill:
				return []ibkrlib.BondContractDetails{synthBondLine(7501, synthDEBill, "EUR", day.AddDate(0, 0, 100))}, nil
			case synthFRBill:
				return []ibkrlib.BondContractDetails{synthBondLine(7502, synthFRBill, "EUR", day.AddDate(0, 0, 50)), synthBondLine(7503, synthFRBill, "EUR", day.AddDate(0, 0, 50))}, nil
			}
			return nil, ibkrlib.ErrContractNoDefinition
		},
		quote: func(context.Context, ibkrlib.BondContractDetails) (rpc.BondQuote, error) {
			return synthLiveQuote(99.4), nil
		},
	}
	res := marketBondCheck(context.Background(), dir, "ISIN", synthDEBill, "EUR", time.Second, now)
	if !res.Resolved || !res.Quoted || res.Contract.ConID != 7501 || res.Contract.Class != rpc.BondClassBill || res.Contract.QuantityUnit != rpc.BondQuantityUnitFace1 ||
		*res.Contract.DaysToMaturity != 100 || *res.Quote.Ask != 99.4 || res.Reason != "" {
		t.Fatalf("resolved = %+v / %+v", res, res.Contract)
	}
	if res := marketBondCheck(context.Background(), dir, "ISIN", synthFRBill, "EUR", time.Second, now); res.Resolved || res.Lines != 2 || !strings.Contains(res.Reason, "ambiguous") {
		t.Fatalf("ambiguous = %+v", res)
	}
	if res := marketBondCheck(context.Background(), dir, "ISIN", synthDEBill2, "EUR", time.Second, now); res.Resolved || !strings.Contains(res.Reason, "no such bond line") {
		t.Fatalf("unknown = %+v", res)
	}
}

// The ledger's SettledCash reaches the account result per currency and the
// sweep's ledger; an absent field stays nil.
func TestLedgerSettledCashReachesTheSweep(t *testing.T) {
	res := &rpc.AccountResult{BaseCurrency: "EUR", CurrencyExposure: []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 12000}, {Currency: "GBP", CashCcy: 5}}}
	annotateLedgerCash(res, map[string]ibkrlib.CurrencyLedger{
		"USD": {CashBalance: 12000, SettledCash: 11500, SettledCashObserved: true, ExchangeRate: 0.9},
		"GBP": {CashBalance: 5, ExchangeRate: 1.1},
		"EUR": {CashBalance: 8000, SettledCash: 7000, SettledCashObserved: true, NetLiquidationByCurrency: 9000, ExchangeRate: 1},
	}, map[string]string{"$LEDGER:CashBalance_USD": "12000", "$LEDGER:CashBalance_GBP": "5", "$LEDGER:CashBalance_EUR": "8000"})
	if s := res.CurrencyExposure[0].SettledCashCcy; s == nil || *s != 11500 {
		t.Fatalf("USD settled = %v", s)
	}
	if res.CurrencyExposure[1].SettledCashCcy != nil || res.BaseCurrencyLedger == nil || *res.BaseCurrencyLedger.SettledCashCcy != 7000 {
		t.Fatalf("GBP %v base %+v", res.CurrencyExposure[1].SettledCashCcy, res.BaseCurrencyLedger)
	}
	res.AccountID = "DU1234567"
	res.Authority = &rpc.AccountDataAuthority{Scope: rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: "paper"}, Availability: rpc.AccountDataAvailable,
		Freshness: rpc.AccountDataFreshnessCurrent, Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}}
	_, ledger, reason := cashSweepLedger(res)
	if reason != "" || ledger["USD"].Settled == nil || *ledger["USD"].Settled != 11500 || ledger["GBP"].Settled != nil || *ledger["EUR"].Settled != 7000 {
		t.Fatalf("sweep ledger = %+v %q", ledger, reason)
	}
}

// The engine reads the test override when one is set, else the server's own
// source, else none.
func TestCashSweepBillSourceSelection(t *testing.T) {
	if (&proposalEngine{}).cashSweepBillSourceFor() != nil {
		t.Fatal("an engine without a server has a bill source")
	}
	s := &Server{}
	if _, ok := (&proposalEngine{server: s}).cashSweepBillSourceFor().(serverBillSource); !ok {
		t.Fatal("the server's own source is not the default")
	}
	fake := &fakeBillSource{}
	s.cashSweepB.source = fake
	if got := (&proposalEngine{server: s}).cashSweepBillSourceFor(); got != cashSweepBillSource(fake) {
		t.Fatal("the override is ignored")
	}
	// The server's own source reads an unread list as unavailable and asks
	// the refresher to run.
	src := serverBillSource{s: &Server{}}
	if _, _, reason := src.usBills(cashSweepTestNow()); !strings.Contains(reason, "not been read") {
		t.Fatalf("unread list = %q", reason)
	}
}
