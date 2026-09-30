package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Bill selection (read-only, internal-docs/design/cash-sweep.md Phase B).
// An invest row names one bill: for USD the outstanding bill from
// TreasuryDirect's list maturing nearest the rung's target inside
// [min_maturity_days, max_maturity_days], resolved at the broker by CUSIP;
// for EUR, GBP and CAD the owner's listed ISINs, resolved and filtered the
// same way. A candidate becomes the row's bill only once contract details
// name exactly one BOND line and a quote carries a price. Otherwise the
// currency reads universe_unavailable (no list to choose from) or
// instrument_unresolved (nothing confirmed), with the evidence, and no row
// exists. A stale quote keeps the row and adds fresh_bill_quote_required.

// cashSweepBillSource is what bill selection reads. The server implements
// it; tests fake it.
type cashSweepBillSource interface {
	// usBills is TreasuryDirect's outstanding list, when it can be served.
	usBills(now time.Time) ([]treasuryBill, time.Time, string)
	// lines are the broker's BOND lines for an identifier in a currency.
	lines(ctx context.Context, idType, id, ccy string) ([]ibkrlib.BondContractDetails, error)
	// quote reads one quote for a line.
	quote(ctx context.Context, line ibkrlib.BondContractDetails) (rpc.BondQuote, error)
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

func (src serverBillSource) lines(ctx context.Context, idType, id, ccy string) ([]ibkrlib.BondContractDetails, error) {
	return src.s.bondDirectory().lookup(ctx, ibkrlib.BondContractRequest{IDType: idType, ID: id, Currency: ccy}, bondDetailsWait)
}

func (src serverBillSource) quote(ctx context.Context, line ibkrlib.BondContractDetails) (rpc.BondQuote, error) {
	return src.s.bondDirectory().quoteFor(ctx, line)
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
		if cp.side != rpc.CashSweepSideInvest || !cashSweepIsBill(cp.instrument) {
			continue
		}
		cashSweepResolveCurrency(ctx, src, bucket.currency(cp.status.Currency), cp, now)
		if i < len(plan.status.Currencies) && plan.status.Currencies[i].Currency == cp.status.Currency {
			plan.status.Currencies[i] = cp.status
		}
	}
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
			lines, err := src.lines(ctx, ibkrlib.BondIdentifierISIN, isin, ccy)
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
			lines, err := src.lines(ctx, cand.idType, cand.id, ccy)
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
		q, err := src.quote(ctx, *line)
		if err != nil {
			evidence = append(evidence, fmt.Sprintf("%s: quote %s", cand.id, bondLookupReason(err)))
			continue
		}
		if !q.HasPrice() {
			evidence = append(evidence, fmt.Sprintf("%s: the quote carried no price (%s)", cand.id, q.StaleReason))
			continue
		}
		bill := cashSweepBillFrom(cand, *line, q)
		cp.bill = &bill
		st.Bill = rpc.CloneCashSweepBill(&bill)
		st.Evidence = nil
		st.Reason += fmt.Sprintf("; the bill is %s maturing %s (%d days), quoted %s per 100 of face", cashSweepBillName(bill), bill.Maturity, bill.DaysToMaturity, formatBillPrice(bill.Price))
		return
	}
	fail(rpc.CashSweepStateInstrumentUnresolved, fmt.Sprintf("no %s bill maturing within %s was confirmed by contract details and a quote", ccy, window), evidence)
}

// cashSweepBillFrom is the resolved bill a row names.
func cashSweepBillFrom(cand cashSweepBillCandidate, line ibkrlib.BondContractDetails, q rpc.BondQuote) rpc.TradeProposalCashSweepBill {
	conv := cashSweepInstrumentConventions[cand.instrument]
	bill := rpc.TradeProposalCashSweepBill{Instrument: cand.instrument, Source: cand.source, ConID: line.ConID, Symbol: line.Symbol,
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
