package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

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
