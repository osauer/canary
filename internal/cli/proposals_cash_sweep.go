package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func renderCashSweepEconomicsAdvice(env *Env, out io.Writer, advice *rpc.CashSweepEconomics) {
	if advice == nil {
		return
	}
	if advice.NetProceeds != nil {
		text := cashSweepMoney(*advice.NetProceeds, advice.Currency) + " (estimated; pending settlement)"
		if advice.RemainingGap != nil && *advice.RemainingGap > 0 {
			text += " · remaining cash gap " + cashSweepMoney(*advice.RemainingGap, advice.Currency)
		}
		statusRow(env, out, "Net proceeds", text)
	}
	if advice.IncrementalGainBase != nil {
		statusRow(env, out, "Benefit (advisory)", cashSweepMoney(*advice.IncrementalGainBase, advice.BaseCurrency))
	}
	if advice.Message != "" {
		statusRow(env, out, "Sweep advice", advice.Message)
	}
	if !advice.AsOf.IsZero() {
		statusRow(env, out, "Advice as of", advice.AsOf.Format("2006-01-02 15:04:05 MST"))
	}
}

// renderCashSweepSection prints the cash sweep under its own heading: the
// mode and the owner's numbers, one band line per currency (with the bill it
// resolved, or the evidence why none), then the rows. A sweep row buys or
// sells a bill rather than protecting a position, so it never sits among the
// protection proposals (internal-docs/design/cash-sweep.md); an active row
// previews and submits like any other.
func renderCashSweepSection(env *Env, out io.Writer, st *rpc.TradeProposalCashSweepStatus, rows []rpc.TradeProposal) {
	if st == nil && len(rows) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Cash sweep  %s\n", formatCashSweepStatus(st, len(rows)))
	if st != nil {
		if o := st.OperationalFunding; o != nil {
			fmt.Fprintf(out, "    Operational funding: %s · %s · gross context, cannot authorise a sweep\n", o.State, o.Source)
			for _, c := range o.Currencies {
				amount := "unavailable"
				if c.GrossPrincipal != nil {
					amount = cashSweepMoney(*c.GrossPrincipal, c.Currency)
				}
				fmt.Fprintf(out, "         %s gross obligations %s · gaps: %s\n", c.Currency, amount, strings.Join(c.Gaps, ", "))
			}
			fmt.Fprintf(out, "         Remaining: %s\n", strings.Join(o.Gaps, ", "))
		}
		for _, study := range st.CalibrationStudies {
			fmt.Fprintf(out, "    %d-session study: %s · %s · %s\n", study.Sessions, study.State, study.Source, strings.Join(study.Gaps, ", "))
		}
		if st.CurrencyPriority != "" {
			fmt.Fprintf(out, "    Priority: %s · existing cash only\n", strings.ReplaceAll(st.CurrencyPriority, "_", " "))
			if st.ReserveCushionEUR != nil {
				fmt.Fprintf(out, "    Total cushion: %s · funding-weighted allocation\n", cashSweepMoney(*st.ReserveCushionEUR, "EUR"))
			}
			if st.ReserveReason != "" {
				fmt.Fprintf(out, "    Reserve: %s\n", st.ReserveReason)
			}
		}
		for _, c := range st.Currencies {
			fmt.Fprintf(out, "    %-4s %-24s %s\n", c.Currency, strings.ReplaceAll(c.State, "_", " "), formatCashSweepCurrency(c))
			for _, line := range formatCashSweepProjection(c.Currency, c.SettlementProjection) {
				fmt.Fprintf(out, "         %-24s %s\n", "", line)
			}
			for _, line := range c.Evidence {
				fmt.Fprintf(out, "         %-24s %s\n", "", line)
			}
		}
		if st.TraceState != "" {
			fmt.Fprintf(out, "    Decision log: %s\n", st.TraceState)
		}
		for i, trace := range st.DecisionTrace {
			if i >= 5 {
				break
			}
			for _, c := range trace.Currencies {
				fmt.Fprintf(out, "    %s · %s · %s · %s\n", trace.At.Format("2006-01-02 15:04 MST"), c.Currency, strings.ReplaceAll(c.Action, "_", " "), c.Reason)
			}
		}
	}
	for _, p := range rows {
		renderProposalRow(env, out, &p)
	}
}

// formatCashSweepStatus is the section header: mode, rows, the order cap
// and the tax review, and what only the owner can write.
func formatCashSweepStatus(st *rpc.TradeProposalCashSweepStatus, rows int) string {
	if st == nil {
		return fmt.Sprintf("%d %s", rows, pluralWord(rows, "row", "rows"))
	}
	parts := []string{st.Mode, fmt.Sprintf("%d %s", rows, pluralWord(rows, "row", "rows"))}
	if st.Shadow && rows > 0 {
		parts = append(parts, "listed for observation; preview and submit refuse them")
	}
	if st.MaxOrderNotionalBase != nil {
		parts = append(parts, "max order "+cashSweepMoney(*st.MaxOrderNotionalBase, st.BaseCurrency))
	}
	if st.MinOrderNotionalBase > 0 {
		parts = append(parts, "min order "+cashSweepMoney(st.MinOrderNotionalBase, st.BaseCurrency))
	}
	if st.MinNetGainBase > 0 {
		parts = append(parts, "benefit benchmark "+cashSweepMoney(st.MinNetGainBase, st.BaseCurrency)+" (advisory)")
	}
	if st.TaxReviewed {
		parts = append(parts, "tax reviewed "+st.TaxReviewedAt)
	} else {
		parts = append(parts, "tax treatment not yet confirmed (advisory)")
	}
	if len(st.NeedsYourNumber) > 0 {
		parts = append(parts, "needs your number: "+strings.Join(st.NeedsYourNumber, ", "))
	}
	if st.Reason != "" {
		parts = append(parts, st.Reason)
	}
	return strings.Join(parts, " · ")
}

// formatCashSweepCurrency is one currency's band: the figures it has, the
// reason, and any number only the owner can write.
func formatCashSweepCurrency(c rpc.TradeProposalCashSweepCurrency) string {
	var parts []string
	money := func(label string, v *float64) {
		if v != nil {
			parts = append(parts, label+" "+cashSweepMoney(*v, c.Currency))
		}
	}
	money("cash", c.Cash)
	money("committed", c.Committed)
	money("funding", c.FundingNeed)
	money("cushion", c.BufferAllocation)
	money("reserve", c.EffectiveReserve)
	if c.Cash != nil {
		parts = append(parts, "keep "+cashSweepMoney(c.KeepCash, c.Currency))
	}
	money("free", c.Free)
	money("equivalents", c.CashEquivalents)
	money("cash-like", c.CashLike)
	if c.SettledCashSource != "" {
		parts = append(parts, "settled cash from "+c.SettledCashSource)
	}
	if c.Reason != "" {
		parts = append(parts, c.Reason)
	}
	if len(c.NeedsYourNumber) > 0 {
		parts = append(parts, "needs your number: "+strings.Join(c.NeedsYourNumber, ", "))
	}
	return strings.Join(parts, " · ")
}

// formatCashSweepProjection keeps historical estimates visibly outside the
// authoritative band. Missing deductions are unavailable, never a zero.
func formatCashSweepProjection(ccy string, p *rpc.CashSweepSettlementProjection) []string {
	if p == nil {
		return nil
	}
	date := p.StatementDate
	if date == "" {
		date = "unavailable"
	}
	state := strings.ReplaceAll(p.State, "_", " ")
	if state == "" {
		state = "unavailable"
	}
	lines := []string{fmt.Sprintf("Flex baseline %s · %s · estimates unverified; cannot authorise a sweep", date, state)}
	if p.BaselineSettledCash != nil || p.KnownPurchases != nil || p.KnownExcludedSales != nil || p.EstimatedCash != nil || p.EstimatedFree != nil {
		value := func(v *float64) string {
			if v == nil {
				return "unavailable"
			}
			return cashSweepMoney(*v, ccy)
		}
		lines = append(lines, "settled baseline "+value(p.BaselineSettledCash)+" · purchase principal deducted "+value(p.KnownPurchases)+" · sale credits excluded "+value(p.KnownExcludedSales)+" · estimated cash "+value(p.EstimatedCash)+" · estimated free "+value(p.EstimatedFree))
	}
	if p.Reason != "" {
		lines = append(lines, p.Reason)
	}
	if len(p.CoverageGaps) > 0 {
		lines = append(lines, "coverage missing: "+strings.Join(p.CoverageGaps, "; "))
	}
	return lines
}

// briefCashValue is the brief's cash line: per currency, cash plus
// equivalents and the sum, "unavailable" where a figure is missing.
func briefCashValue(row *rpc.BriefCashRow) string {
	if row == nil {
		return ""
	}
	figure := func(v *float64, ccy string) string {
		if v == nil {
			return "unavailable"
		}
		return cashSweepMoney(*v, ccy)
	}
	var parts []string
	for _, c := range row.Currencies {
		parts = append(parts, fmt.Sprintf("%s cash %s + equivalents %s = %s", c.Currency,
			figure(c.Cash, c.Currency), figure(c.CashEquivalents, c.Currency), figure(c.CashLike, c.Currency)))
	}
	return strings.Join(parts, " · ")
}

// cashSweepMoney prints an observed amount; an observed zero is zero, so it
// is never drawn as the em-dash placeholder other tables use for "none".
func cashSweepMoney(v float64, ccy string) string {
	if strings.TrimSpace(ccy) == "" {
		return fmt.Sprintf("%.2f", v)
	}
	return formatMoneyCcyForPnL(v, ccy)
}
