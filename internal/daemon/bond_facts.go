package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// Bill and bond facts for the console (internal-docs/design/bond-risk.md,
// "Bill and bond facts"). For every classified bill or bond the positions
// read states, in the line's currency: the price it trades at (the quote's
// bid, else the broker's mark) and its yield on a named basis; the yield's
// change since the prior close; what ±25 bp would do to the position; what
// it pays at maturity; and its clean gain since purchase split into carry,
// rates and spread. Everything is computed here from broker prices, Canary's
// order journal and, for the spread, a benchmark bill's quote and Canary's
// own daily yield records; nothing is read as zero. The read never waits on
// the gateway: quotes and lookups it lacks are requested detached and the
// next read carries them.

const (
	// bondFactsLookupWait is how long the facts wait for a line already
	// being looked up; the classification has usually filled the cache.
	bondFactsLookupWait = 100 * time.Millisecond
	// bondFactsScenarioBp is the parallel yield move the console shows.
	bondFactsScenarioBp = 25.0
	// bondYieldRecordsScope and bondYieldRecordsKind name the daemon.db
	// document that keeps one yield record per held bill and day, with the
	// benchmark's yield that day, so a spread since purchase can be stated.
	bondYieldRecordsScope = "bond-facts:yield-records"
	bondYieldRecordsKind  = "bond_yield_records_v1"
	// bondYieldRecordsKeep is how long a line's records outlive its last
	// observation.
	bondYieldRecordsKeep = 400 * 24 * time.Hour
)

// bondBenchmarkInstrument names the sovereign bill a vocabulary bill's
// yield is compared against: a BTF against the Bubill nearest in maturity.
// US bills are the USD benchmark and the Bubill the EUR one; UK and
// Canadian bills have no benchmark defined.
var bondBenchmarkInstrument = map[string]string{cashSweepInstrumentFRBTF: cashSweepInstrumentDEBubill}

// bondCurrencyZone is the calendar zone of a currency's bills when the
// instrument's own session is unknown.
var bondCurrencyZone = map[string]string{"USD": "America/New_York", "EUR": "Europe/Berlin", "GBP": "Europe/London", "CAD": "America/Toronto"}

// fillBondFacts fills each classified line's Facts. Rows supply the broker's
// mark, market value and unrealised gain; the directory supplies lines and
// cached quotes; the journal supplies purchase lots; the records supply the
// benchmark yield at purchase.
func (s *Server) fillBondFacts(ctx context.Context, bonds []rpc.PositionBond, rows []rpc.PositionView, now time.Time) {
	if s == nil || len(bonds) == 0 {
		return
	}
	byConID := map[int]rpc.PositionView{}
	for _, row := range rows {
		if row.ConID > 0 {
			byConID[row.ConID] = row
		}
	}
	dir := s.bondDirectory()
	records := s.bondYieldRecords()
	views, events := s.bondFactsOrderViews()
	scope := s.currentBrokerStateScope()
	in := bondFactsInput{dir: dir, records: records, views: views, events: events, scope: scope, held: bonds, policyISINs: s.bondFactsPolicyISINs(), now: now}
	for i := range bonds {
		b := &bonds[i]
		if b.Class != rpc.BondClassBill && b.Class != rpc.BondClassBond {
			continue
		}
		b.Facts = bondFactsFor(ctx, in, b, byConID[b.ConID])
	}
	records.save(ctx, s.coreStore, now)
}

// bondFactsInput is what one positions read hands every line's facts.
type bondFactsInput struct {
	dir         *bondDirectory
	records     *bondYieldRecords
	views       []rpc.OrderView
	events      map[string][]rpc.OrderEvent
	scope       brokerStateScope
	held        []rpc.PositionBond
	policyISINs []string
	now         time.Time
}

// bondFactsOrderViews loads the journal's order views once per read; a
// journal that cannot be read leaves purchases to the broker's cost.
func (s *Server) bondFactsOrderViews() ([]rpc.OrderView, map[string][]rpc.OrderEvent) {
	if s.orderJournal == nil {
		return nil, nil
	}
	views, events, err := s.loadOrderViews()
	if err != nil {
		return nil, nil
	}
	return views, events
}

// bondFactsPolicyISINs are the EUR bills the sweep policy names, the
// benchmark candidates beyond the held book.
func (s *Server) bondFactsPolicyISINs() []string {
	if s.protectionPolicies == nil {
		return nil
	}
	policy, _ := s.protectionPolicies.Active()
	bucket := policy.Cash.Sweep
	if bucket == nil {
		return nil
	}
	return slices.Clone(bucket.currency("EUR").ISINs)
}

// bondFactsFor states one line's facts.
func bondFactsFor(ctx context.Context, in bondFactsInput, b *rpc.PositionBond, row rpc.PositionView) *rpc.BondFacts {
	f := &rpc.BondFacts{Currency: b.Currency, PriceConvention: rpc.BondPriceConventionPer100, ScenarioModel: rpc.BondScenarioModel}
	instrument := cashSweepHeldBillInstrument(*b, b.Currency)
	today := cashSweepDay(in.now)
	maturity, err := time.Parse(time.DateOnly, b.Maturity)
	if err != nil || !maturity.After(today) {
		f.Unavailable = "the maturity is unknown or has passed"
		return f
	}
	days := cashSweepDaysLeft(today, maturity)

	// Face and what it pays at maturity.
	facePerUnit := 0.0
	if conv, ok := cashSweepInstrumentConventions[instrument]; ok && conv.FacePerUnit > 0 {
		facePerUnit = conv.FacePerUnit
	}
	face, faceSource := bondFace(b, row, facePerUnit)
	if face != nil {
		f.FaceValue, f.FaceSource = face, faceSource
		f.PaysAtMaturity = &rpc.BondMaturityPayment{Amount: *face, Currency: b.Currency, Date: b.Maturity, Days: days}
		if facePerUnit == 0 && b.Quantity != 0 {
			facePerUnit = *face / math.Abs(b.Quantity)
		}
	}

	// The quote and the price the facts are read at.
	line := bondFactsLine(ctx, in.dir, b, row, instrument)
	quote, haveQuote := in.dir.cachedQuote(b.ConID)
	if !haveQuote || quote.AsOf.Before(in.now.Add(-bondQuoteTTL)) {
		in.dir.refreshQuoteDetached(line)
		if !haveQuote {
			f.QuoteNote = "quote requested; the next read carries it"
		}
	}
	if haveQuote {
		f.Bid, f.Ask, f.Last, f.Close = quote.Bid, quote.Ask, quote.Last, quote.Close
		f.QuoteAsOf, f.QuoteFresh = quote.AsOf, quote.Fresh
		if !quote.Fresh && quote.StaleReason != "" {
			f.QuoteNote = quote.StaleReason
		}
	}
	f.Mark = ptrIfPos(b.Mark)
	switch {
	case f.Bid != nil:
		f.Price, f.PriceSource = f.Bid, rpc.BondPriceSourceBid
	case f.Mark != nil:
		f.Price, f.PriceSource = f.Mark, rpc.BondPriceSourceMark
	default:
		f.Unavailable = "no bid and no mark: the price is unknown"
		return f
	}
	price := *f.Price

	// Yield, the scenario and the change since the prior close.
	coupon := 0.0
	if b.Coupon != nil {
		coupon = *b.Coupon
	}
	if b.Class == rpc.BondClassBond && b.Coupon == nil {
		f.Unavailable = "the coupon is unknown, so no yield can be read"
		return f
	}
	model := bondFactsModel{class: b.Class, currency: b.Currency, coupon: coupon, maturity: maturity}
	y, err := model.yield(price, today)
	if err != nil {
		f.Unavailable = "the yield cannot be read: " + err.Error()
		return f
	}
	f.YieldPct, f.YieldBasis, f.YieldBasisNote = &y, model.basis(), model.basisNote()
	if face != nil {
		up, down, dv01 := model.scenario(*face, price, today)
		f.ValueChangeUp25Bp, f.ValueChangeDown25Bp, f.DV01 = &up, &down, &dv01
	}
	closeDay, closeNote := bondPriorSessionDay(instrument, b.Currency, in.now)
	f.PriorCloseDate, f.PriorCloseNote = closeDay.Format(time.DateOnly), closeNote
	switch {
	case f.Close == nil:
		f.YieldChangeUnavailable = "the broker sent no prior close"
	default:
		if closeYield, err := model.yield(*f.Close, closeDay); err != nil {
			f.YieldChangeUnavailable = "the yield at the prior close cannot be read: " + err.Error()
		} else {
			f.PriorCloseYieldPct = &closeYield
			f.YieldChangeBp = new((y - closeYield) * 100)
		}
	}

	// Since purchase.
	if face == nil {
		f.SincePurchaseUnavailable = "the face value is unknown"
		return f
	}
	purchase := bondPurchaseLots(in, b, row, facePerUnit, *face)
	f.Purchase = purchase.view(model, maturity)
	if purchase.source == "" {
		f.SincePurchaseUnavailable = purchase.note
		return f
	}
	pnl := &rpc.BondPnLSplit{Currency: b.Currency, Scope: rpc.BondPnLScopeCleanPrice}
	f.PnL = pnl
	if len(purchase.lots) == 0 {
		// A price without a date: the broker's gain is the total and
		// nothing more can be split.
		pnl.Total, pnl.TotalSource = row.UnrealizedPnL, rpc.BondPnLTotalSourceBroker
		pnl.SpreadUnavailable = nonEmptyString(bondBenchmarkStaticReason(b.Class, instrument), "the purchase date is unknown")
		f.SincePurchaseUnavailable = purchase.note
		return f
	}
	bench := bondBenchmarkFor(ctx, in, b, instrument, maturity, model, today)
	pnl.Benchmark = bench.view
	benchNow := bench.yieldNow
	if bench.view != nil && bench.view.YieldPct != nil {
		bench.view.SpreadBpNow = new((y - *bench.view.YieldPct) * 100)
	}
	lots := purchase.lots
	if benchNow != nil {
		lots = in.records.benchmarkAtPurchase(b.ConID, lots, bench)
	}
	split, err := model.split(lots, price, today, benchNow)
	if err != nil {
		f.PnL = nil
		f.SincePurchaseUnavailable = "the gain cannot be split: " + err.Error()
		return f
	}
	pnl.Total, pnl.TotalSource = split.Total, rpc.BondPnLTotalSourceLots
	pnl.Carry, pnl.Rates, pnl.Spread = &split.Carry, &split.Rates, split.Spread
	f.Purchase.YieldPct = &split.PurchaseYieldPct
	if pnl.Spread == nil {
		pnl.SpreadUnavailable = nonEmptyString(bench.unavailable, "no benchmark yield is on record for the purchase date")
	}
	// The record for today: the line's yield and, when read, the benchmark's.
	if b.Class == rpc.BondClassBill {
		in.records.note(b.ConID, today, bondYieldRecord{Date: today.Format(time.DateOnly), YieldPct: y, Basis: f.YieldBasis,
			BenchmarkConID: bench.conID, BenchmarkISIN: bench.isin, BenchmarkYieldPct: benchNow})
	}
	return f
}

// bondFace is the line's face value: quantity × the instrument's face per
// unit, else the broker's market value over its mark (a line whose unit
// Canary does not know). Nil when neither can be read.
func bondFace(b *rpc.PositionBond, row rpc.PositionView, facePerUnit float64) (*float64, string) {
	if facePerUnit > 0 && b.Quantity != 0 {
		return new(math.Abs(b.Quantity) * facePerUnit), rpc.BondFaceSourceQuantityUnit
	}
	if b.Mark > 0 && row.MarketValue != 0 {
		return new(math.Abs(row.MarketValue) * 100 / b.Mark), rpc.BondFaceSourceMarketValue
	}
	return nil, ""
}

// bondFactsLine is the line a quote is read for: the directory's when it
// has one, else the position's own identity (contract id, type, currency).
func bondFactsLine(ctx context.Context, dir *bondDirectory, b *rpc.PositionBond, row rpc.PositionView, instrument string) ibkrlib.BondContractDetails {
	if dir != nil && b.ConID > 0 {
		lines, err := dir.lookup(ctx, ibkrlib.BondContractRequest{ConID: b.ConID, Currency: b.Currency, SecTypes: cashSweepHeldSecTypes(row.SecType, instrument)}, bondFactsLookupWait)
		if err == nil {
			if line, _, err := bondLineFor(lines, b.Currency, b.ConID); err == nil {
				return line
			}
		}
	}
	line := ibkrlib.BondContractDetails{ConID: b.ConID, Symbol: b.Symbol, SecType: ibkrlib.BillOrBondSecType(row.SecType), Currency: b.Currency, SecIDs: map[string]string{}}
	if b.ISIN != "" {
		line.SecIDs[ibkrlib.BondIdentifierISIN] = b.ISIN
	}
	if b.CUSIP != "" {
		line.SecIDs[ibkrlib.BondIdentifierCUSIP] = b.CUSIP
	}
	return line
}

// bondFactsModel is the yield convention one line's facts are read on: a
// bill's simple money-market yield on its currency's day count, or a
// coupon bond's yield to maturity (risk.MeasureBond).
type bondFactsModel struct {
	class, currency string
	coupon          float64
	maturity        time.Time
}

func (m bondFactsModel) bill() bool { return m.class == rpc.BondClassBill }

func (m bondFactsModel) basis() string {
	if m.bill() {
		basis, _ := risk.BillYieldBasis(m.currency)
		return basis
	}
	return risk.BondYieldBasis(m.currency)
}

func (m bondFactsModel) basisNote() string {
	if m.bill() {
		_, count := risk.BillYieldBasis(m.currency)
		return fmt.Sprintf("simple annualised yield from price to par, actual/%d", int(count))
	}
	return fmt.Sprintf("yield to maturity compounded %d× a year, days over 365.25", risk.BondCouponsPerYear(m.currency))
}

// yield is the line's yield in percent at a price per 100 on a day.
func (m bondFactsModel) yield(price float64, day time.Time) (float64, error) {
	if m.bill() {
		_, count := risk.BillYieldBasis(m.currency)
		return risk.BillYieldPct(price, cashSweepDaysLeft(cashSweepDay(day), m.maturity), count)
	}
	measure, err := risk.MeasureBond(price, m.coupon, m.maturity, day, risk.BondCouponsPerYear(m.currency))
	if err != nil {
		return 0, err
	}
	return measure.YieldPct, nil
}

// scenario is the value change on face for a 25 bp rise and fall and the
// value lost per 1 bp rise, at a price on a day.
func (m bondFactsModel) scenario(face, price float64, day time.Time) (up, down, dv01 float64) {
	if m.bill() {
		_, count := risk.BillYieldBasis(m.currency)
		days := cashSweepDaysLeft(cashSweepDay(day), m.maturity)
		y, err := risk.BillYieldPct(price, days, count)
		if err != nil {
			return 0, 0, 0
		}
		return risk.BillScenario(face, y, days, count, bondFactsScenarioBp), risk.BillScenario(face, y, days, count, -bondFactsScenarioBp),
			-risk.BillScenario(face, y, days, count, 1)
	}
	measure, err := risk.MeasureBond(price, m.coupon, m.maturity, day, risk.BondCouponsPerYear(m.currency))
	if err != nil {
		return 0, 0, 0
	}
	value := face * price / 100
	return -value * measure.LossFraction(bondFactsScenarioBp/100), -value * measure.LossFraction(-bondFactsScenarioBp/100), value * measure.LossFraction(0.01)
}

// split is the clean gain since purchase in its parts.
func (m bondFactsModel) split(lots []risk.BondLot, price float64, day time.Time, benchNow *float64) (risk.BondPnLSplit, error) {
	if m.bill() {
		_, count := risk.BillYieldBasis(m.currency)
		return risk.SplitBillPnL(lots, price, cashSweepDaysLeft(cashSweepDay(day), m.maturity), count, m.maturity, benchNow)
	}
	return risk.SplitBondPnL(lots, price, m.coupon, m.maturity, day, risk.BondCouponsPerYear(m.currency))
}

// bondPriorSessionDay is the weekday before today in the line's calendar
// zone: the day the broker's prior close belongs to. Exchange holidays are
// not checked, and the note says so.
func bondPriorSessionDay(instrument, currency string, now time.Time) (time.Time, string) {
	zone := cashSweepInstrumentConventions[instrument].TimeZone
	if zone == "" {
		zone = bondCurrencyZone[normCcy(currency)]
	}
	loc := time.UTC
	if zone != "" {
		if l, err := time.LoadLocation(zone); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, -1)
	}
	return day, "the prior weekday's close as the broker sent it; exchange holidays are not checked"
}

// bondPurchase is where a line's purchase came from: lots with their own
// dates (the journal), a price without a date (the broker's cost), or
// nothing, with the note saying why.
type bondPurchase struct {
	lots        []risk.BondLot
	source      string
	note        string
	pricePer100 float64
	firstDate   time.Time
}

// view is the purchase as the console reads it.
func (p bondPurchase) view(model bondFactsModel, maturity time.Time) *rpc.BondPurchase {
	if p.source == "" {
		return nil
	}
	out := &rpc.BondPurchase{PricePer100: p.pricePer100, Lots: len(p.lots), Source: p.source, Note: p.note}
	if !p.firstDate.IsZero() {
		out.Date = p.firstDate.Format(time.DateOnly)
	}
	return out
}

// bondPurchaseLots reads the line's purchase. Every filled buy of the
// contract in the journal, in this account and mode, is a lot with its own
// fill price and first fill day; the lots are used when their face matches
// the held face. Otherwise the broker's average cost, read back from its
// market value, mark and unrealised gain (unit-free), gives the price, and
// the journal's earliest buy, when it has one, the date.
func bondPurchaseLots(in bondFactsInput, b *rpc.PositionBond, row rpc.PositionView, facePerUnit, heldFace float64) bondPurchase {
	var lots []risk.BondLot
	var first time.Time
	journalFace, weighted := 0.0, 0.0
	for _, view := range in.views {
		if view.ConID != b.ConID || !strings.EqualFold(strings.TrimSpace(view.Action), "BUY") || !(view.Filled > 0) || !(view.AvgFillPrice > 0) {
			continue
		}
		if !brokerScopedAccountMatches(view.Account, in.scope) || !brokerScopedModeMatches(view.Mode, in.scope.Mode) {
			continue
		}
		if brokerScopeAccountConcrete(view.Account) && brokerScopeAccountConcrete(in.scope.Account) && !strings.EqualFold(strings.TrimSpace(view.Account), strings.TrimSpace(in.scope.Account)) {
			continue
		}
		at := bondFirstFillTime(in.events[orderViewKey(view)], view)
		if at.IsZero() || at.After(in.now) {
			continue
		}
		lotFace := view.Filled * facePerUnit
		lots = append(lots, risk.BondLot{Face: lotFace, PricePer100: view.AvgFillPrice, Date: cashSweepDay(at)})
		journalFace += lotFace
		weighted += lotFace * view.AvgFillPrice
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	if len(lots) > 0 && facePerUnit > 0 && math.Abs(journalFace-heldFace) <= math.Max(1, heldFace*0.005) {
		return bondPurchase{lots: lots, source: rpc.BondPurchaseSourceJournal, pricePer100: weighted / journalFace, firstDate: cashSweepDay(first)}
	}
	// The broker's cost, unit-free: cost = market value − unrealised gain,
	// so the price paid per 100 is mark × (1 − gain / market value).
	if !(b.Mark > 0) || row.MarketValue == 0 {
		return bondPurchase{note: "the broker's average cost cannot be read without a mark and a market value"}
	}
	price := b.Mark * (1 - row.UnrealizedPnL/row.MarketValue)
	if !(price > 0) || math.IsNaN(price) || math.IsInf(price, 0) {
		return bondPurchase{note: "the broker's average cost does not read as a price per 100"}
	}
	out := bondPurchase{source: rpc.BondPurchaseSourceBrokerCost, pricePer100: price}
	switch {
	case len(lots) > 0:
		out.firstDate = cashSweepDay(first)
		out.lots = []risk.BondLot{{Face: heldFace, PricePer100: price, Date: out.firstDate}}
		out.note = fmt.Sprintf("the journal's %d buy(s) cover %.0f of %.0f face; the broker's average cost prices the position from the first buy's day", len(lots), journalFace, heldFace)
	default:
		out.note = "the purchase date is unknown: the line was not bought through Canary, so the gain cannot be split"
	}
	return out
}

// bondFirstFillTime is the earliest fill the journal holds for an order:
// the broker's execution time when it sent one, else the receipt.
func bondFirstFillTime(events []rpc.OrderEvent, view rpc.OrderView) time.Time {
	var first time.Time
	for _, ev := range events {
		if !(ev.Filled > 0) || (ev.Type != orderJournalEventStatusUpdated && ev.Type != orderJournalEventBrokerAcknowledged) {
			continue
		}
		at := ev.At
		if exec, ok := parseIBKRExecTime(ev.ExecTime); ok {
			at = exec
		}
		if !at.IsZero() && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	if first.IsZero() {
		first = view.UpdatedAt
	}
	return first.UTC()
}

// bondBenchmark is the benchmark read for one line: its identity and yield
// now, or why there is none.
type bondBenchmark struct {
	conID       int
	isin        string
	yieldNow    *float64
	view        *rpc.BondBenchmark
	unavailable string
}

// bondBenchmarkStaticReason says why a line has no benchmark at all, before
// any lookup: a coupon bond, a bill outside the vocabulary, a bill that is
// itself its currency's benchmark, or one with no benchmark defined. Empty
// when a benchmark instrument exists.
func bondBenchmarkStaticReason(class, instrument string) string {
	_, ok := bondBenchmarkInstrument[instrument]
	switch {
	case class != rpc.BondClassBill:
		return "no benchmark is read for a coupon bond"
	case instrument == "":
		return "the bill is not one Canary knows a benchmark for"
	case instrument == cashSweepInstrumentUSTBill:
		return "US Treasury bills are the USD benchmark; spread does not apply"
	case instrument == cashSweepInstrumentDEBubill:
		return "Bubills are the EUR benchmark; spread does not apply"
	case !ok:
		return fmt.Sprintf("no benchmark instrument is defined for %s", instrument)
	}
	return ""
}

// bondBenchmarkFor finds the benchmark bill nearest in maturity, among the
// held book and the sweep policy's EUR bills, and reads its cached quote.
// Only the lines' cached lookups and quotes are read; what is missing is
// requested detached, within the same market-data budget as any bond quote.
func bondBenchmarkFor(ctx context.Context, in bondFactsInput, b *rpc.PositionBond, instrument string, maturity time.Time, model bondFactsModel, today time.Time) bondBenchmark {
	if reason := bondBenchmarkStaticReason(b.Class, instrument); reason != "" {
		return bondBenchmark{unavailable: reason}
	}
	benchInstrument := bondBenchmarkInstrument[instrument]
	conv := cashSweepInstrumentConventions[benchInstrument]
	ccy := cashSweepBillCurrency[benchInstrument]
	type candidate struct {
		line ibkrlib.BondContractDetails
		gap  time.Duration
	}
	var best *candidate
	consider := func(line ibkrlib.BondContractDetails) {
		lineMaturity, ok := line.MaturityDate()
		if !ok || !lineMaturity.After(today) || line.ConID == b.ConID {
			return
		}
		gap := lineMaturity.Sub(maturity)
		if gap < 0 {
			gap = -gap
		}
		if best == nil || gap < best.gap || (gap == best.gap && line.ConID < best.line.ConID) {
			best = &candidate{line: line, gap: gap}
		}
	}
	pending := 0
	for _, held := range in.held {
		if held.Class != rpc.BondClassBill || held.ConID == b.ConID || cashSweepHeldBillInstrument(held, held.Currency) != benchInstrument {
			continue
		}
		lines, err := in.dir.lookup(ctx, ibkrlib.BondContractRequest{ConID: held.ConID, Currency: ccy, SecTypes: conv.SecTypes}, bondFactsLookupWait)
		if err != nil {
			pending++
			continue
		}
		if line, _, err := bondLineFor(lines, ccy, held.ConID); err == nil {
			consider(line)
		}
	}
	for _, isin := range in.policyISINs {
		isin = strings.ToUpper(strings.TrimSpace(isin))
		if cashSweepIdentifierInstrument(ibkrlib.BondIdentifierISIN, isin) != benchInstrument {
			continue
		}
		lines, err := in.dir.lookup(ctx, ibkrlib.BondContractRequest{IDType: ibkrlib.BondIdentifierISIN, ID: isin, Currency: ccy, SecTypes: conv.SecTypes}, bondFactsLookupWait)
		if err != nil {
			pending++
			continue
		}
		if line, _, err := bondLineFor(lines, ccy, 0); err == nil {
			consider(line)
		}
	}
	if best == nil {
		if pending > 0 {
			return bondBenchmark{unavailable: fmt.Sprintf("%d benchmark %s lookup(s) still running; the next read may carry one", pending, conv.SessionLabel)}
		}
		return bondBenchmark{unavailable: fmt.Sprintf("no %s is held or named in the sweep policy to serve as the benchmark", conv.SessionLabel)}
	}
	line := best.line
	view := &rpc.BondBenchmark{ConID: line.ConID, ISIN: line.ISIN(), Issuer: nonEmptyString(line.LongName, line.DescAppend), YieldBasis: model.basis(), Source: benchInstrument}
	if m, ok := line.MaturityDate(); ok {
		view.Maturity = m.Format(time.DateOnly)
	}
	out := bondBenchmark{conID: line.ConID, isin: view.ISIN, view: view}
	quote, have := in.dir.cachedQuote(line.ConID)
	if !have || quote.AsOf.Before(in.now.Add(-bondQuoteTTL)) {
		in.dir.refreshQuoteDetached(line)
	}
	if !have {
		out.unavailable = fmt.Sprintf("benchmark %s quote requested; the next read carries it", view.ISIN)
		view.Note = out.unavailable
		return out
	}
	view.QuoteAsOf = quote.AsOf
	price, source := 0.0, ""
	switch {
	case quote.Bid != nil:
		price, source = *quote.Bid, rpc.BondPriceSourceBid
	case quote.Ask != nil:
		price, source = *quote.Ask, rpc.BondPriceSourceAsk
	case quote.Last != nil:
		price, source = *quote.Last, rpc.BondPriceSourceLast
	default:
		out.unavailable = fmt.Sprintf("benchmark %s quote carries no price: %s", view.ISIN, nonEmptyString(quote.StaleReason, "no price arrived"))
		view.Note = out.unavailable
		return out
	}
	benchModel := bondFactsModel{class: rpc.BondClassBill, currency: ccy}
	benchModel.maturity, _ = line.MaturityDate()
	y, err := benchModel.yield(price, today)
	if err != nil {
		out.unavailable = fmt.Sprintf("benchmark %s yield cannot be read: %v", view.ISIN, err)
		view.Note = out.unavailable
		return out
	}
	view.YieldPct = &y
	view.Note = "yield read at the benchmark's " + source
	if !quote.Fresh && quote.StaleReason != "" {
		view.Note += "; " + quote.StaleReason
	}
	out.yieldNow = &y
	return out
}

// cachedQuote returns the directory's most recent quote for a contract id,
// however old, without reading one; the caller judges its age by AsOf.
func (d *bondDirectory) cachedQuote(conID int) (rpc.BondQuote, bool) {
	if d == nil {
		return rpc.BondQuote{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.quotes[conID]
	if !ok || e.err != nil && !e.quote.HasPrice() {
		return rpc.BondQuote{}, false
	}
	return *rpc.CloneBondQuote(&e.quote), true
}

// refreshQuoteDetached reads a quote for a line in the background, one read
// per line at a time, so a positions read never waits on the gateway.
func (d *bondDirectory) refreshQuoteDetached(line ibkrlib.BondContractDetails) {
	if d == nil || line.ConID <= 0 {
		return
	}
	d.mu.Lock()
	if d.quoteInflight == nil {
		d.quoteInflight = map[int]bool{}
	}
	if d.quoteInflight[line.ConID] || d.quote == nil {
		d.mu.Unlock()
		return
	}
	d.quoteInflight[line.ConID] = true
	d.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), bondQuoteTimeout+2*time.Second)
		defer cancel()
		_, _ = d.quoteFor(ctx, line)
		d.mu.Lock()
		delete(d.quoteInflight, line.ConID)
		d.mu.Unlock()
	}()
}

// bondYieldRecord is one day's yield of a held bill on its basis and, when
// read that day, the benchmark's.
type bondYieldRecord struct {
	Date              string   `json:"date"`
	YieldPct          float64  `json:"yield_pct"`
	Basis             string   `json:"basis"`
	BenchmarkConID    int      `json:"benchmark_con_id,omitempty"`
	BenchmarkISIN     string   `json:"benchmark_isin,omitempty"`
	BenchmarkYieldPct *float64 `json:"benchmark_yield_pct,omitempty"`
}

// bondYieldRecordsDocument is the persisted shape: records by contract id,
// each list in date order.
type bondYieldRecordsDocument struct {
	Lines map[string][]bondYieldRecord `json:"lines"`
}

// bondYieldRecords keeps the daily yield records and persists them when
// they change.
type bondYieldRecords struct {
	mu     sync.Mutex
	loaded bool
	doc    bondYieldRecordsDocument
	dirty  bool
}

// bondYieldRecords returns the server's records, loading them from daemon.db
// on first use.
func (s *Server) bondYieldRecords() *bondYieldRecords {
	b := s.bondSupport()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.yields == nil {
		b.yields = &bondYieldRecords{}
	}
	r := b.yields
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.loaded {
		r.loaded = true
		if raw, ok, err := loadMarketState(s.coreStore, bondYieldRecordsScope, bondYieldRecordsKind); err == nil && ok {
			_ = json.Unmarshal(raw, &r.doc)
		}
	}
	if r.doc.Lines == nil {
		r.doc.Lines = map[string][]bondYieldRecord{}
	}
	return r
}

// note records today's yield for a line, replacing an earlier record of the
// same day; a benchmark yield already on record for the day is kept when
// the new record lacks one.
func (r *bondYieldRecords) note(conID int, day time.Time, rec bondYieldRecord) {
	if r == nil || conID <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strconv.Itoa(conID)
	list := r.doc.Lines[key]
	for i := range list {
		if list[i].Date != rec.Date {
			continue
		}
		if rec.BenchmarkYieldPct == nil && list[i].BenchmarkYieldPct != nil {
			rec.BenchmarkConID, rec.BenchmarkISIN, rec.BenchmarkYieldPct = list[i].BenchmarkConID, list[i].BenchmarkISIN, list[i].BenchmarkYieldPct
		}
		if list[i] != rec && !(list[i].BenchmarkYieldPct != nil && rec.BenchmarkYieldPct != nil && *list[i].BenchmarkYieldPct == *rec.BenchmarkYieldPct &&
			list[i].YieldPct == rec.YieldPct && list[i].BenchmarkConID == rec.BenchmarkConID) {
			list[i] = rec
			r.dirty = true
		}
		return
	}
	r.doc.Lines[key] = append(list, rec)
	slices.SortFunc(r.doc.Lines[key], func(a, b bondYieldRecord) int { return strings.Compare(a.Date, b.Date) })
	r.dirty = true
}

// benchmarkAtPurchase gives each lot the benchmark yield on record for its
// date, or the first record after it (the spread part then runs from that
// day, which the benchmark view names). A lot with no record keeps nil, so
// the split states no spread.
func (r *bondYieldRecords) benchmarkAtPurchase(conID int, lots []risk.BondLot, bench bondBenchmark) []risk.BondLot {
	if r == nil || bench.view == nil {
		return lots
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(lots)
	var since string
	for i := range out {
		date := out[i].Date.Format(time.DateOnly)
		for _, rec := range r.doc.Lines[strconv.Itoa(conID)] {
			if rec.BenchmarkYieldPct == nil || rec.Date < date {
				continue
			}
			out[i].BenchmarkPct = rec.BenchmarkYieldPct
			if rec.Date > date {
				// The record postdates the purchase: the line's own yield that
				// day is the reference, so the move before it counts as rates.
				out[i].ReferenceYieldPct = new(rec.YieldPct)
				if since == "" || rec.Date > since {
					since = rec.Date
				}
			}
			if bench.view.SpreadBpThen == nil {
				bench.view.SpreadBpThen = new((rec.YieldPct - *rec.BenchmarkYieldPct) * 100)
			}
			break
		}
	}
	if since != "" {
		bench.view.SpreadSince = since
		bench.view.Note = nonEmptyString(bench.view.Note+"; ", "") + "the spread part runs from " + since + ", the first benchmark record after the purchase"
	}
	return out
}

// save persists the records when they changed, pruning lines unseen for
// bondYieldRecordsKeep. A missing store keeps them in memory only.
func (r *bondYieldRecords) save(ctx context.Context, store *corestore.Store, now time.Time) {
	if r == nil || store == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return
	}
	cutoff := now.UTC().Add(-bondYieldRecordsKeep).Format(time.DateOnly)
	for key, list := range r.doc.Lines {
		if len(list) == 0 || list[len(list)-1].Date < cutoff {
			delete(r.doc.Lines, key)
		}
	}
	raw, err := json.Marshal(r.doc)
	if err != nil {
		return
	}
	if saveMarketDocument(ctx, store, bondYieldRecordsScope, bondYieldRecordsKind, raw) == nil {
		r.dirty = false
	}
}
