package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// renderCashSweepSection prints the cash sweep under its own heading: the
// mode and the owner's numbers, one band line per currency (with the bill it
// resolved, or the evidence why none), then the rows. The rows are
// observation until bill orders are authorised, so they never sit among the
// protection proposals (internal-docs/design/cash-sweep.md).
func renderCashSweepSection(env *Env, out io.Writer, st *rpc.TradeProposalCashSweepStatus, rows []rpc.TradeProposal) {
	if st == nil && len(rows) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Cash sweep  %s\n", formatCashSweepStatus(st, len(rows)))
	if st != nil {
		for _, c := range st.Currencies {
			fmt.Fprintf(out, "    %-4s %-24s %s\n", c.Currency, strings.ReplaceAll(c.State, "_", " "), formatCashSweepCurrency(c))
			for _, line := range c.Evidence {
				fmt.Fprintf(out, "         %-24s %s\n", "", line)
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
