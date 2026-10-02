package daemon

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// annotateLedgerCash records which ledger rows carried a cash balance and
// adds the base currency's own ledger row, which CurrencyExposure leaves out.
// The cash sweep reads cash per currency only through these two
// (internal-docs/design/cash-sweep.md): a zero that was never observed must
// not read as zero cash.
func annotateLedgerCash(res *rpc.AccountResult, ledger map[string]ibkrlib.CurrencyLedger, raw map[string]string) {
	if res == nil {
		return
	}
	for i := range res.CurrencyExposure {
		res.CurrencyExposure[i].CashObserved = ledgerCashObserved(raw, res.CurrencyExposure[i].Currency)
		res.CurrencyExposure[i].SettledCashCcy = ledgerSettledCash(ledger, res.CurrencyExposure[i].Currency)
	}
	base := normCcy(res.BaseCurrency)
	if base == "" {
		return
	}
	for ccy, row := range ledger {
		if normCcy(ccy) != base {
			continue
		}
		res.BaseCurrencyLedger = &rpc.CurrencyExposure{
			Currency:             base,
			NetLiquidationCcy:    row.NetLiquidationByCurrency,
			CashCcy:              row.CashBalance,
			StockMarketValueCcy:  row.StockMarketValue,
			OptionMarketValueCcy: row.OptionMarketValue,
			UnrealizedPnLCcy:     row.UnrealizedPnL,
			RealizedPnLCcy:       row.RealizedPnL,
			ExchangeRate:         1,
			NetLiquidationBase:   row.NetLiquidationByCurrency,
			CashObserved:         ledgerCashObserved(raw, base),
			SettledCashCcy:       ledgerSettledCash(ledger, base),
		}
		return
	}
}

// settledCashScheduleReconcileTolerance is how far, in currency units, a
// schedule's final balance may sit from current trade-date cash and still
// describe the same account state. TWS rounds stream cash to whole units; a gap
// within this bound moves the admitted low by no more than the gap itself.
const settledCashScheduleReconcileTolerance = 1.0

// annotateSettledCashSchedules judges TWS's per-currency settled-cash
// schedules (SettledCashByDate) from the account stream against the snapshot's
// trade-date cash. A schedule that ends at current trade-date cash describes
// the current account state, so its lowest balance is the most the currency
// can spend without a settled debit on any date. A mismatch (a fill TWS has
// not re-sent yet, a truncated schedule), a stream that is not completely
// downloaded or an invalid value is held, with the reason.
func annotateSettledCashSchedules(res *rpc.AccountResult, capture *ibkrlib.SettledCashScheduleCapture) {
	if res == nil || capture == nil {
		return
	}
	apply := func(row *rpc.CurrencyExposure) {
		if s, ok := capture.Schedules[normCcy(row.Currency)]; ok {
			row.SettledCashSchedule = judgeSettledCashSchedule(*row, s, capture)
		}
	}
	for i := range res.CurrencyExposure {
		apply(&res.CurrencyExposure[i])
	}
	if res.BaseCurrencyLedger != nil {
		apply(res.BaseCurrencyLedger)
	}
}

func judgeSettledCashSchedule(row rpc.CurrencyExposure, s ibkrlib.SettledCashSchedule, capture *ibkrlib.SettledCashScheduleCapture) *rpc.SettledCashSchedule {
	out := &rpc.SettledCashSchedule{Status: rpc.SettledCashScheduleHeld, ReceivedAt: s.ReceivedAt.UTC(),
		Points: settledCashPoints(s.Points), SegmentPoints: settledCashPoints(s.SegmentPoints)}
	ccy := normCcy(row.Currency)
	switch {
	case capture.StreamStatus != "initial_complete":
		out.Reason = "the TWS account stream is " + strings.ReplaceAll(capture.StreamStatus, "_", " ")
	case capture.Truncated:
		out.Reason = "TWS sent more settlement schedules than Canary keeps"
	case s.Status != "observed" || len(s.Points) == 0:
		out.Reason = "TWS's " + ccy + " settlement schedule is invalid: " + nonEmptyString(s.Reason, "no points")
	case !row.CashObserved || !finiteProtectionOptionPolicyValue(row.CashCcy):
		out.Reason = "no current " + ccy + " trade-date cash to reconcile TWS's settlement schedule with"
	case math.Abs(s.Points[len(s.Points)-1].Amount-row.CashCcy) > settledCashScheduleReconcileTolerance:
		out.Reason = "TWS's " + ccy + " settlement schedule does not end at current trade-date cash; waiting for TWS's next account update"
	default:
		low := s.Points[0].Amount
		for _, p := range slices.Concat(s.Points, s.SegmentPoints) {
			low = min(low, p.Amount)
		}
		out.Status, out.Low = rpc.SettledCashScheduleAdmitted, new(low)
	}
	return out
}

func settledCashPoints(in []ibkrlib.SettledCashPoint) []rpc.SettledCashPoint {
	if len(in) == 0 {
		return nil
	}
	out := make([]rpc.SettledCashPoint, 0, len(in))
	for _, p := range in {
		out = append(out, rpc.SettledCashPoint{Date: p.Date.Format(time.DateOnly), Amount: p.Amount})
	}
	return out
}

// ledgerSettledCash is a currency's SettledCash from the typed ledger, nil
// when the gateway sent none. That $LEDGER:ALL sends it per currency is an
// assumption the post-install proof checks (internal-docs/design/cash-sweep.md).
func ledgerSettledCash(ledger map[string]ibkrlib.CurrencyLedger, ccy string) *float64 {
	ccy = normCcy(ccy)
	for key, row := range ledger {
		if normCcy(key) == ccy && ccy != "" && row.SettledCashObserved {
			return new(row.SettledCash)
		}
	}
	return nil
}

// ledgerCashObserved reports whether the raw account summary carried a
// parsable CashBalance for ccy in any ledger dialect: the bare tag, the
// gateway's "$LEDGER-" prefix, or the one-shot storage namespace all end in
// "CashBalance_<CCY>". CashBalance is never an account-level tag, so the
// suffix alone identifies a ledger cash row.
func ledgerCashObserved(raw map[string]string, ccy string) bool {
	ccy = normCcy(ccy)
	if ccy == "" {
		return false
	}
	suffix := "CASHBALANCE_" + ccy
	for k, v := range raw {
		if !strings.HasSuffix(strings.ToUpper(k), suffix) {
			continue
		}
		if _, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return true
		}
	}
	return false
}

// cashLikeByCurrency is cash and classified cash equivalents per currency,
// for the brief's cash row and rule 14's evidence. It is served only while
// the cash sweep is enabled, so the brief and the Rulebook change for nobody
// who has not opted in; ok reports that. reason is set when the account
// ledger itself is unavailable. Equivalents are nil while positions are not
// current or a holding cannot be classified; nil is unavailable, never zero.
func (s *Server) cashLikeByCurrency(acct *rpc.AccountResult, pos *rpc.PositionsResult, positionsCurrent bool) (rows []rpc.BriefCashCurrency, reason string, ok bool) {
	if s == nil || s.protectionPolicies == nil {
		return nil, "", false
	}
	policy, _ := s.protectionPolicies.Active()
	bucket := policy.Buckets.CashSweep
	if !bucket.enabled() {
		return nil, "", false
	}
	return cashLikeRows(bucket, acct, pos, positionsCurrent)
}

func cashLikeRows(bucket *protectionCashSweepPolicy, acct *rpc.AccountResult, pos *rpc.PositionsResult, positionsCurrent bool) ([]rpc.BriefCashCurrency, string, bool) {
	_, ledger, reason := cashSweepLedger(acct)
	if reason != "" {
		return nil, reason, true
	}
	holdings, unclassified := cashSweepClassify(bucket, pos)
	in := cashSweepInput{Ledger: ledger, Holdings: holdings, Unclassified: unclassified}
	rows := []rpc.BriefCashCurrency{}
	for _, ccy := range cashSweepCurrencies(bucket, in) {
		row := rpc.BriefCashCurrency{Currency: ccy}
		if l, ok := ledger[ccy]; ok && l.Observed {
			row.Cash = new(l.TradeDate)
		}
		if positionsCurrent && nonEmptyString(unclassified[ccy], unclassified[""]) == "" {
			equivalents := 0.0
			for _, h := range holdings[ccy] {
				equivalents += h.MarketValue
			}
			row.CashEquivalents = new(equivalents)
		}
		if row.Cash != nil && row.CashEquivalents != nil {
			row.CashLike = new(*row.Cash + *row.CashEquivalents)
		}
		rows = append(rows, row)
	}
	return rows, "", true
}

// briefCashRow is the Ready movement's cash-like row; nil while the cash
// sweep is not enabled.
func (s *Server) briefCashRow(acct *rpc.AccountResult, pos *rpc.PositionsResult, positionsCurrent bool) *rpc.BriefCashRow {
	rows, reason, ok := s.cashLikeByCurrency(acct, pos, positionsCurrent)
	if !ok {
		return nil
	}
	if reason != "" {
		return &rpc.BriefCashRow{BriefRowState: briefUnavailable(reason), Currencies: []rpc.BriefCashCurrency{}}
	}
	out := &rpc.BriefCashRow{BriefRowState: briefOK("cash and classified cash equivalents per currency, each in its own unit (trade-date cash)"), Currencies: rows}
	for _, row := range rows {
		if row.CashLike == nil {
			out.BriefRowState = briefDegraded("a currency's cash or cash equivalents are unavailable; nothing is read as zero")
			break
		}
	}
	return out
}

// rulebookCashLike converts the cash-like rows for rule 14's evidence; nil
// while the cash sweep is not enabled or the ledger is unavailable.
func (s *Server) rulebookCashLike(acct *rpc.AccountResult, pos *rpc.PositionsResult, positionsCurrent bool) []risk.CurrencyCashLike {
	rows, reason, ok := s.cashLikeByCurrency(acct, pos, positionsCurrent)
	if !ok || reason != "" {
		return nil
	}
	out := make([]risk.CurrencyCashLike, 0, len(rows))
	for _, row := range rows {
		out = append(out, risk.CurrencyCashLike{Currency: row.Currency, Cash: cloneFloat64Ptr(row.Cash), Equivalents: cloneFloat64Ptr(row.CashEquivalents)})
	}
	return out
}

// cashSweepNeedsYourNumber lists, for canary policy status, what an enabled
// cash sweep still needs from the owner. An absent or disabled sweep is
// opt-in and asks for nothing.
func cashSweepNeedsYourNumber(p *protectionCashSweepPolicy) []string {
	if !p.enabled() {
		return nil
	}
	var out []string
	if missing := p.missingNumbers(); len(missing) > 0 {
		out = append(out, "cash sweep: holds until you write "+strings.Join(missing, ", ")+" in [buckets.cash_sweep]")
	}
	for _, ccy := range slices.Sorted(maps.Keys(p.Currency)) {
		if missing := p.Currency[ccy].missingNumbers(); len(missing) > 0 {
			out = append(out, fmt.Sprintf("cash sweep: the %s ETF needs %s in [buckets.cash_sweep.currency.%s]", ccy, strings.Join(missing, ", "), ccy))
		}
	}
	if _, written := p.Currency["EUR"]; !written {
		out = append(out, "cash sweep: the EUR fallback ETF needs etf_symbol, etf_exchange in [buckets.cash_sweep.currency.EUR]; bills still plan")
	}
	if p.TaxReviewedAt == "" {
		out = append(out, "cash sweep: tax treatment not yet confirmed; write tax_reviewed_at in [buckets.cash_sweep] once reviewed (advisory, blocks nothing)")
	}
	return out
}
