package daemon

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Bill selection (internal-docs/design/cash-sweep.md Phase B). An invest row
// names one bill: for USD the outstanding bill from TreasuryDirect's list
// maturing nearest the rung's target inside [min_maturity_days,
// max_maturity_days], resolved at the broker by CUSIP; for EUR, GBP and CAD
// the owner's listed ISINs, resolved and filtered the same way. Each is asked
// as its instrument's security types (A10: BILL for US bills; BILL then
// BOND for the others). A candidate becomes the row's bill only once
// contract details name exactly one BILL or BOND line with its size rules
// and minimum tick and a quote carries a price;
// the row's quantity is then sized in the bill's order unit on that grid.
// Otherwise the currency reads universe_unavailable (no list to choose from)
// or instrument_unresolved (nothing confirmed), with the evidence, and no
// row exists. A stale quote keeps the row and adds
// fresh_bill_quote_required. A redemption reads its held bill's line by
// contract id for the same grid and its session.

// cashSweepBillSource is what bill selection reads. The server implements
// it; tests fake it.
type cashSweepBillSource interface {
	// usBills is TreasuryDirect's outstanding list, when it can be served.
	usBills(now time.Time) ([]treasuryBill, time.Time, string)
	// lines are the broker's lines for an identifier in a currency, asked as
	// secTypes in order (BILL, BOND).
	lines(ctx context.Context, idType, id, ccy string, secTypes []string) ([]ibkrlib.BondContractDetails, error)
	// quote reads one quote for a line.
	quote(ctx context.Context, line ibkrlib.BondContractDetails) (rpc.BondQuote, error)
	// held are the broker's lines for a held contract id, asked as secTypes.
	held(ctx context.Context, conID int, ccy string, secTypes []string) ([]ibkrlib.BondContractDetails, error)
}

// cashSweepResolveBudget bounds bill selection per refresh, so a slow
// gateway cannot stall the proposal cycle; lookups keep running detached and
// the next cycle finds them.
const cashSweepResolveBudget = 10 * time.Second

// cashSweepMaxBillAttempts bounds how many candidates one currency confirms
// per cycle, nearest first.
const cashSweepMaxBillAttempts = 3

// serverBillSource reads the daemon's own list, directory and quotes.
type serverBillSource struct{ s *Server }

func (src serverBillSource) usBills(now time.Time) ([]treasuryBill, time.Time, string) {
	u := src.s.billUniverse()
	bills, at, reason := u.snapshot(now)
	if reason != "" {
		u.requestRefresh()
	}
	return bills, at, reason
}

func (src serverBillSource) lines(ctx context.Context, idType, id, ccy string, secTypes []string) ([]ibkrlib.BondContractDetails, error) {
	return src.s.bondDirectory().lookup(ctx, ibkrlib.BondContractRequest{IDType: idType, ID: id, Currency: ccy, SecTypes: secTypes}, bondDetailsWait)
}

func (src serverBillSource) quote(ctx context.Context, line ibkrlib.BondContractDetails) (rpc.BondQuote, error) {
	return src.s.bondDirectory().quoteFor(ctx, line)
}

func (src serverBillSource) held(ctx context.Context, conID int, ccy string, secTypes []string) ([]ibkrlib.BondContractDetails, error) {
	return src.s.bondDirectory().lookup(ctx, ibkrlib.BondContractRequest{ConID: conID, Currency: ccy, SecTypes: secTypes}, bondDetailsWait)
}

// cashSweepBillSourceFor is the bill source the engine reads: the test
// override, else the server's own, else none.
func (e *proposalEngine) cashSweepBillSourceFor() cashSweepBillSource {
	if e == nil || e.server == nil {
		return nil
	}
	b := e.server.bondSupport()
	b.mu.Lock()
	override := b.source
	b.mu.Unlock()
	if override != nil {
		return override
	}
	return serverBillSource{s: e.server}
}

// cashSweepBillCandidate is one bill a currency could buy.
type cashSweepBillCandidate struct {
	idType, id, instrument, source string
	maturity                       time.Time
	days                           int
	// line is set when the candidate was found by resolving it (an owner's
	// ISIN); a TreasuryDirect bill is resolved when it is tried.
	line *ibkrlib.BondContractDetails
}

// cashSweepResolveBills names the bill every invest currency buys, or turns
// the currency into universe_unavailable or instrument_unresolved. It keeps
// plan.status.Currencies in step with plan.currencies.
func cashSweepResolveBills(ctx context.Context, src cashSweepBillSource, bucket *protectionCashSweepPolicy, plan *cashSweepPlan, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, cashSweepResolveBudget)
	defer cancel()
	for i := range plan.currencies {
		cp := &plan.currencies[i]
		switch {
		case cp.side == rpc.CashSweepSideInvest && cashSweepIsBill(cp.instrument):
			cashSweepResolveCurrency(ctx, src, bucket.currency(cp.status.Currency), cp, now)
		case cp.side == rpc.CashSweepSideInvest:
			// The declared ETF is matched by contract id, which Canary does not
			// resolve: there is nothing it could order, so there is no row.
			cp.side = ""
			cp.status.State = rpc.CashSweepStateInstrumentUnresolved
			cp.status.Reason = "the declared ETF is matched by contract id, which Canary does not resolve yet; no ETF order is proposed and the cash stays cash"
		case cp.side == rpc.CashSweepSideRedeem && cp.holding != nil && cashSweepIsBill(cp.holding.Instrument):
			cashSweepResolveRedemption(ctx, src, cp, now)
		}
		if i < len(plan.status.Currencies) && plan.status.Currencies[i].Currency == cp.status.Currency {
			plan.status.Currencies[i] = cp.status
		}
	}
}

// cashSweepResolveRedemption reads the held bill's line by contract id for
// its order grid and session and puts the sale on the grid. A line without
// the rules, or a sale the grid refuses, keeps the row and blocks it: the
// shortfall below keep_cash is news the owner needs even when Canary cannot
// order the sale.
func cashSweepResolveRedemption(ctx context.Context, src cashSweepBillSource, cp *cashSweepCurrencyPlan, now time.Time) {
	h := cp.holding
	ccy := cp.status.Currency
	held := int(math.Floor(h.Row.Quantity + 1e-9))
	cp.units = cp.quantity
	var line *ibkrlib.BondContractDetails
	if src != nil {
		if lines, err := src.held(ctx, h.Row.ConID, ccy, cashSweepHeldSecTypes(h.Row.SecType, h.Instrument)); err == nil {
			if l, _, err := bondLineFor(lines, ccy, h.Row.ConID); err == nil {
				line = &l
			}
		}
	}
	cp.session = cashSweepBondSession(line, h.Instrument, now)
	if line == nil {
		cp.blockers = append(cp.blockers, rpc.TradingBlocker{Code: rpc.CashSweepBlockerBillRules,
			Message: "the held bill's contract details cannot be read now, so the sale cannot be put on its size and price grid",
			Action:  "Refresh once the gateway answers; the row then sizes the sale."})
		return
	}
	rules, err := ibkrlib.BondOrderRulesFrom(*line)
	if err != nil {
		cp.blockers = append(cp.blockers, rpc.TradingBlocker{Code: rpc.CashSweepBlockerBillRules,
			Message: "the held bill's contract details carry no minimum size or minimum tick (" + err.Error() + "), so the sale cannot be sized",
			Action:  "Sell by hand if cash must be raised; the row sizes the sale once the details carry the rules."})
		return
	}
	cp.rules = &rules
	units, reason := cashSweepRedeemUnits(cp.quantity, min(held, max(cp.capUnits, 1)), rules)
	if units == 0 {
		cp.blockers = append(cp.blockers, rpc.TradingBlocker{Code: rpc.CashSweepBlockerBelowMinimum, Message: reason,
			Action: "Sell by hand if cash must be raised; Canary sends only orders on the bill's size grid."})
		return
	}
	cp.units = units
}

func cashSweepResolveCurrency(ctx context.Context, src cashSweepBillSource, cfg protectionCashSweepCurrency, cp *cashSweepCurrencyPlan, now time.Time) {
	st := &cp.status
	ccy := st.Currency
	today := cashSweepDay(now)
	fail := func(state, reason string, evidence []string) {
		cp.side, cp.bill = "", nil
		st.State, st.Reason, st.Evidence, st.Bill = state, reason, evidence, nil
	}
	if src == nil {
		fail(rpc.CashSweepStateInstrumentUnresolved, "no gateway is attached, so no bill can be resolved or quoted", nil)
		return
	}
	window := fmt.Sprintf("%d–%d days", cfg.MinMaturityDays, cfg.MaxMaturityDays)
	var candidates []cashSweepBillCandidate
	var evidence []string
	if ccy == "USD" {
		bills, fetchedAt, reason := src.usBills(now)
		if reason != "" {
			fail(rpc.CashSweepStateUniverseUnavailable, reason+"; USD bills are chosen from that list", nil)
			return
		}
		for _, bill := range bills {
			issue, okIssue := treasuryDirectDate(bill.IssueDate)
			maturity, okMaturity := treasuryDirectDate(bill.MaturityDate)
			if !okIssue || !okMaturity || issue.After(today) {
				continue
			}
			if days := cashSweepDaysLeft(today, maturity); days >= cfg.MinMaturityDays && days <= cfg.MaxMaturityDays {
				candidates = append(candidates, cashSweepBillCandidate{idType: ibkrlib.BondIdentifierCUSIP, id: bill.CUSIP,
					instrument: cashSweepInstrumentUSTBill, source: rpc.CashSweepBillSourceTreasuryDirect, maturity: maturity, days: days})
			}
		}
		if len(candidates) == 0 {
			fail(rpc.CashSweepStateInstrumentUnresolved, fmt.Sprintf("none of the %d bills in TreasuryDirect's list (read %s) is issued and matures within %s",
				len(bills), fetchedAt.UTC().Format(time.RFC3339), window), nil)
			return
		}
	} else {
		if len(cfg.ISINs) == 0 {
			fail(rpc.CashSweepStateUniverseUnavailable, fmt.Sprintf("no isins are listed in [buckets.cash_sweep.currency.%s]; list the %s bills the sweep may buy", ccy, ccy), nil)
			return
		}
		plannable := cashSweepPlannable(cfg)
		for _, isin := range cfg.ISINs {
			instrument := cashSweepISINCountryInstrument[isin[:min(2, len(isin))]]
			if !slices.Contains(plannable, instrument) {
				evidence = append(evidence, fmt.Sprintf("%s: not an instrument declared for %s", isin, ccy))
				continue
			}
			lines, err := src.lines(ctx, ibkrlib.BondIdentifierISIN, isin, ccy, cashSweepInstrumentSecTypes(instrument))
			if err != nil {
				evidence = append(evidence, fmt.Sprintf("%s: contract details %s", isin, bondLookupReason(err)))
				continue
			}
			line, _, err := bondLineFor(lines, ccy, 0)
			if err != nil {
				evidence = append(evidence, fmt.Sprintf("%s: %v", isin, err))
				continue
			}
			maturity, ok := line.MaturityDate()
			if !ok {
				evidence = append(evidence, fmt.Sprintf("%s: the contract carries no maturity", isin))
				continue
			}
			days := cashSweepDaysLeft(today, maturity)
			if days < cfg.MinMaturityDays || days > cfg.MaxMaturityDays {
				evidence = append(evidence, fmt.Sprintf("%s: matures %s (%d days), outside %s", isin, maturity.Format(time.DateOnly), days, window))
				continue
			}
			candidates = append(candidates, cashSweepBillCandidate{idType: ibkrlib.BondIdentifierISIN, id: isin, instrument: instrument,
				source: rpc.CashSweepBillSourcePolicyISINs, maturity: maturity, days: days, line: &line})
		}
		if len(candidates) == 0 {
			fail(rpc.CashSweepStateInstrumentUnresolved, fmt.Sprintf("none of the %d listed %s isins resolves to a bill maturing within %s", len(cfg.ISINs), ccy, window), evidence)
			return
		}
	}
	slices.SortFunc(candidates, func(a, b cashSweepBillCandidate) int {
		return cmp.Or(cmp.Compare(cashSweepAbs(a.days-cp.targetDays), cashSweepAbs(b.days-cp.targetDays)), cmp.Compare(a.days, b.days), strings.Compare(a.id, b.id))
	})
	for _, cand := range candidates[:min(len(candidates), cashSweepMaxBillAttempts)] {
		line := cand.line
		if line == nil {
			lines, err := src.lines(ctx, cand.idType, cand.id, ccy, cashSweepInstrumentSecTypes(cand.instrument))
			if err != nil {
				evidence = append(evidence, fmt.Sprintf("%s: contract details %s", cand.id, bondLookupReason(err)))
				continue
			}
			resolved, _, err := bondLineFor(lines, ccy, 0)
			if err != nil {
				evidence = append(evidence, fmt.Sprintf("%s: %v", cand.id, err))
				continue
			}
			if maturity, ok := resolved.MaturityDate(); !ok || !maturity.Equal(cand.maturity) {
				evidence = append(evidence, fmt.Sprintf("%s: the broker's maturity %s differs from the list's %s", cand.id, resolved.Maturity, cand.maturity.Format(time.DateOnly)))
				continue
			}
			line = &resolved
		}
		rules, err := ibkrlib.BondOrderRulesFrom(*line)
		if err != nil {
			evidence = append(evidence, fmt.Sprintf("%s: %v, so no order can be sized", cand.id, err))
			continue
		}
		q, err := src.quote(ctx, *line)
		if err != nil {
			evidence = append(evidence, fmt.Sprintf("%s: quote %s", cand.id, bondLookupReason(err)))
			continue
		}
		if !q.HasPrice() {
			evidence = append(evidence, fmt.Sprintf("%s: the quote carried no price (%s)", cand.id, q.StaleReason))
			continue
		}
		bill := cashSweepBillFrom(cand, *line, q, now)
		st.Evidence = nil
		st.Bill = rpc.CloneCashSweepBill(&bill)
		st.Reason += fmt.Sprintf("; the bill is %s maturing %s (%d days), quoted %s per 100 of face", cashSweepBillName(bill), bill.Maturity, bill.DaysToMaturity, formatBillPrice(bill.Price))
		conv := cashSweepInstrumentConventions[bill.Instrument]
		units, reason := cashSweepInvestUnits(cp.orderAmount, conv, rules, *bill.Price, ccy)
		if units == 0 {
			cp.side = ""
			st.State, st.Reason = rpc.CashSweepStateHold, st.Reason+"; "+reason+", so nothing is swept"
			return
		}
		cp.bill, cp.rules, cp.units, cp.session = &bill, &rules, units, rpc.CloneBondSession(bill.Session)
		st.Reason += fmt.Sprintf("; the order is %d × %s (%s of face)", units, conv.QuantityUnit, formatBudgetMoney(float64(units)*conv.FacePerUnit, ccy))
		return
	}
	fail(rpc.CashSweepStateInstrumentUnresolved, fmt.Sprintf("no %s bill maturing within %s was confirmed by contract details and a quote", ccy, window), evidence)
}

// cashSweepBillFrom is the resolved bill a row names.
func cashSweepBillFrom(cand cashSweepBillCandidate, line ibkrlib.BondContractDetails, q rpc.BondQuote, now time.Time) rpc.TradeProposalCashSweepBill {
	conv := cashSweepInstrumentConventions[cand.instrument]
	bill := rpc.TradeProposalCashSweepBill{Instrument: cand.instrument, Source: cand.source, ConID: line.ConID, SecType: ibkrlib.BillOrBondSecType(line.SecType), Symbol: line.Symbol,
		ISIN: line.ISIN(), CUSIP: line.CUSIP(), Maturity: cand.maturity.Format(time.DateOnly), DaysToMaturity: cand.days,
		Quote: rpc.CloneBondQuote(&q), QuoteFresh: q.Fresh, QuantityUnit: conv.QuantityUnit, PriceConvention: conv.PriceConvention}
	switch cand.idType {
	case ibkrlib.BondIdentifierISIN:
		bill.ISIN = cand.id
	case ibkrlib.BondIdentifierCUSIP:
		bill.CUSIP = cand.id
	}
	if line.Complete {
		bill.MinSize, bill.SizeIncrement = ptrIfPos(line.MinSize), ptrIfPos(line.SizeIncrement)
	}
	bill.MinTick = ptrIfPos(line.MinTick)
	bill.Session = cashSweepBondSession(&line, cand.instrument, now)
	for _, p := range []struct {
		name string
		v    *float64
	}{{"ask", q.Ask}, {"last", q.Last}, {"bid", q.Bid}, {"close", q.Close}} {
		if p.v != nil {
			bill.Price, bill.PriceSource = new(*p.v), p.name
			break
		}
	}
	return bill
}

// cashSweepBillName names a bill by the identifier it was chosen by.
func cashSweepBillName(b rpc.TradeProposalCashSweepBill) string {
	if b.Source == rpc.CashSweepBillSourceTreasuryDirect && b.CUSIP != "" {
		return "CUSIP " + b.CUSIP
	}
	if b.ISIN != "" {
		return "ISIN " + b.ISIN
	}
	return "CUSIP " + b.CUSIP
}

func formatBillPrice(v *float64) string {
	if v == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.4f", *v)
}
