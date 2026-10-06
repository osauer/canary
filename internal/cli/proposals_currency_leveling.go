package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// renderCurrencyLevelingSection prints currency leveling under its own
// heading: the owner's numbers, one line per currency, then each repayment
// with its conversion rows in send order. A conversion repays a margin loan
// rather than protecting a position, so it never sits among the protection
// proposals (internal-docs/design/currency-leveling.md); a repayment is
// approved as a whole, on the owner's approval.
func renderCurrencyLevelingSection(env *Env, out io.Writer, st *rpc.TradeProposalCurrencyLevelingStatus, rows []rpc.TradeProposal) {
	if st == nil && len(rows) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Currency leveling  %s\n", formatCurrencyLevelingStatus(st, len(rows)))
	if st != nil {
		for _, c := range st.Currencies {
			fmt.Fprintf(out, "    %-4s %-17s %s\n", c.Currency, strings.ReplaceAll(c.State, "_", " "), formatCurrencyLevelingCurrency(c))
		}
	}
	byKey := map[string]rpc.TradeProposal{}
	for _, p := range rows {
		byKey[p.Key] = p
	}
	if st != nil {
		for _, b := range st.Bundles {
			fmt.Fprintf(out, "    %s\n", formatCurrencyLevelingBundle(b, st.BaseCurrency))
			for _, key := range b.Keys {
				if p, ok := byKey[key]; ok {
					renderProposalRow(env, out, &p)
					delete(byKey, key)
				}
			}
		}
	}
	for _, p := range rows {
		if _, ok := byKey[p.Key]; ok {
			renderProposalRow(env, out, &p)
		}
	}
}

// currencyLevelingSendOrder returns the leveling rows with each repayment's
// conversions in its send order (bundles[].keys), then any row no bundle
// names in producer order.
func currencyLevelingSendOrder(st *rpc.TradeProposalCurrencyLevelingStatus, rows []rpc.TradeProposal) []rpc.TradeProposal {
	if st == nil {
		return rows
	}
	byKey := map[string]rpc.TradeProposal{}
	for _, p := range rows {
		byKey[p.Key] = p
	}
	out := make([]rpc.TradeProposal, 0, len(rows))
	for _, b := range st.Bundles {
		for _, key := range b.Keys {
			if p, ok := byKey[key]; ok {
				out = append(out, p)
				delete(byKey, key)
			}
		}
	}
	for _, p := range rows {
		if _, ok := byKey[p.Key]; ok {
			out = append(out, p)
		}
	}
	return out
}

// formatCurrencyLevelingBundle is one repayment's line: the loan, how many
// conversions one approval sends, and what it saves against what it can
// cost within the payback window.
func formatCurrencyLevelingBundle(b rpc.TradeProposalCurrencyLevelingBundle, base string) string {
	n := len(b.Keys)
	return fmt.Sprintf("Repay %s: %d %s, one approval · saves about %s within %d days · costs at most %s",
		b.Currency, n, pluralWord(n, "conversion", "conversions"), cashSweepMoney(b.SavingBase, base), b.PaybackDays, cashSweepMoney(b.CostBase, base))
}

// formatCurrencyLevelingStatus is the section header: rows, the band,
// cushion and price bound, the order cap in force, and what only the owner
// can write.
func formatCurrencyLevelingStatus(st *rpc.TradeProposalCurrencyLevelingStatus, rows int) string {
	parts := []string{fmt.Sprintf("%d %s", rows, pluralWord(rows, "row", "rows"))}
	if rows > 0 {
		parts = append(parts, "each repayment needs your approval")
	}
	if st == nil {
		return strings.Join(parts, " · ")
	}
	if st.TriggerBase != nil {
		parts = append(parts, "band "+cashSweepMoney(*st.TriggerBase, st.BaseCurrency))
	}
	if st.CushionBase != nil {
		parts = append(parts, "cushion "+cashSweepMoney(*st.CushionBase, st.BaseCurrency))
	}
	if st.MaxSlippageBP != nil {
		parts = append(parts, "limit within "+strconv.FormatFloat(*st.MaxSlippageBP, 'f', -1, 64)+" bp of the mid")
	}
	if st.PaybackDays != nil {
		parts = append(parts, fmt.Sprintf("a conversion pays back within %d days", *st.PaybackDays))
	}
	if st.OrderCapBase != nil {
		parts = append(parts, "one repayment at most "+cashSweepMoney(*st.OrderCapBase, st.BaseCurrency)+" (order cap in force)")
	}
	switch {
	case st.RatesReason != "":
		parts = append(parts, st.RatesReason)
	case st.RatesThrough != "":
		parts = append(parts, "interest rates from the broker's statements to "+st.RatesThrough)
	}
	if len(st.NeedsYourNumber) > 0 {
		parts = append(parts, "needs your number: "+strings.Join(st.NeedsYourNumber, ", "))
	}
	if st.Reason != "" {
		parts = append(parts, st.Reason)
	}
	return strings.Join(parts, " · ")
}

// formatCurrencyLevelingCurrency is one currency's line: the daemon's reason,
// which already names the cash; the bare cash when there is no reason.
func formatCurrencyLevelingCurrency(c rpc.TradeProposalCurrencyLevelingCurrency) string {
	if c.Reason != "" || c.Cash == nil {
		return c.Reason
	}
	return "cash " + cashSweepMoney(*c.Cash, c.Currency)
}
