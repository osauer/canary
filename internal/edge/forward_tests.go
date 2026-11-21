package edge

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ForwardTest is one rule registered before the results it will be read on.
// A reading taken after the fact can be fitted to what already happened; a
// rule written down first, with the time it was written, cannot. The daemon
// records per install when each rule was first registered (the rule text is
// identified by RuleHash), and only decisions executed after that instant
// belong to the rule's sample.
//
// The rules are frozen as registered. Editing a Rule changes its hash, which
// registers the new text beside the old one with its own, later instant: the
// old sample is never relabelled, and the other rules keep their dates.
// Canary registers the rules and keeps what a later read needs; it does not
// read the tests and takes no verdict, grade or order behaviour from them.
type ForwardTest struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Rule  string `json:"rule"`
}

// RuleHash identifies the registered text. It covers the ID and the Rule,
// not the Title.
func (t ForwardTest) RuleHash() string {
	sum := sha256.Sum256([]byte("edge-forward-test-v1\x00" + t.ID + "\x00" + t.Rule))
	return "rule_" + hex.EncodeToString(sum[:16])
}

// InForwardSample says whether a decision executed at executedAt belongs to
// the sample of a rule registered at registeredAt: strictly after it. A
// decision at the registration instant or before is hindsight for the rule,
// whatever was learned about it later.
func InForwardSample(registeredAt, executedAt time.Time) bool {
	return !registeredAt.IsZero() && executedAt.After(registeredAt)
}

// ForwardTests returns the seven registered rules in their fixed order. It
// carries rules only and no result. The texts are frozen as shipped: an
// edited text registers again with a later instant.
func ForwardTests() []ForwardTest {
	return []ForwardTest{
		{
			ID:    "stock_sales_vs_nearby_day",
			Title: "Stock sales against later days",
			Rule:  "One reading per name and week in which a stock or ETF was sold out of a long position: the name's move over the five sessions after the sale day's close, weighted by the size of each sale where a week holds several. The figure is the mean over the readings. It is compared with the same mean when each sale is moved to a random session within the 20 sessions after it (2,000 draws). The test holds if the figure is below the 10th percentile of the draws in both the earlier and the later half of the readings by date, which means the name was weaker after the sales than after later days. Read once, at 80 readings.",
		},
		{
			ID:    "entries_vs_nearby_day",
			Title: "Entries against a nearby day",
			Rule:  "One reading per name and week in which a long position in a stock, an ETF or a single-name option was opened or added to: the underlying's move net of SPY over the five sessions after the entry day's close, counted negative for a bought put and averaged over the week's entries. A single name is any underlying but SPY and VIX. The figure is the mean over the readings. It is compared with the same mean when each entry is moved to a random session within 20 sessions either side (2,000 draws). The test holds if the figure is positive and above the 90th percentile of the draws in both the earlier and the later half of the readings by date. Read once, at 167 readings.",
		},
		{
			ID:    "option_buys_below_premium_line",
			Title: "Option buys below the premium line against above it",
			Rule:  "Option buys that open or add to a long position, made on days when premium at risk at the prior statement was at or below the line, against such buys made above it. One reading per contract and week, final when its position has closed: the average price of the position's sales after the buy, zero at expiry, less the price paid, on the contracts bought, after the buy's costs. A position closed by delivery gives no reading. The figure for each group is the sum of its readings divided by the premium paid for them. The line is the Rulebook's premium budget act level (premium_budget_act_pct, percent of net liquidation) in the regime set the Rulebook applied that day, read from the policy file. The line is not supported if the group at or below it returns no more on its premium than the group above it. Read once, at 30 final readings at or below the line.",
		},
		{
			ID:    "option_buys_after_regime_warning",
			Title: "Single-name option buys after a Regime warning session",
			Rule:  "Single-name option buys that open or add to a long position, made in the session after a Regime warning, against all other such buys. A single name is any underlying but SPY and VIX. One reading per contract and week, final when its position has closed: the average price of the position's sales after the buy, zero at expiry, less the price paid, on the contracts bought, after the buy's costs. A position closed by delivery gives no reading. The figure for each group is the sum of its readings divided by the premium paid for them. The label is Regime's stage of the session before the buy: warning is early warning, confirmed stress or panic; the rest is quiet, stabilization or opportunity. No pass threshold is set for this test; it is read by direction in both the earlier and the later half of the readings by date. Read once, at 40 final readings after a warning session that fall on at least 20 separate days.",
		},
		{
			ID:    "final_option_sales_vs_close_5_sessions",
			Title: "Final option sales against the close five sessions later",
			Rule:  "One reading per position, single names only: the contract's close five US sessions after the sale that closed the position, against the price taken, on the contracts of that sale. A single name is any underlying but SPY and VIX. Two groups, fixed now: sold above the position's average cost, and sold below it. Counted only where a closing quote is stored for that day, and the figure says it is a quote; this leaves out anything within five sessions of expiry. No reading before 30 positions in a group. It counts as a rule only if one group's average move, in percent of the price taken, has the same sign in the earlier and the later half of its readings by date and its 95% interval, resampled over positions, leaves out zero. The sample is sales made after registration; a close fetched for an earlier sale is hindsight and stays out.",
		},
		{
			ID:    "floor_after_partial_sale",
			Title: "A floor under what is left after a partial sale",
			Rule:  "Long single-name option positions with a partial sale after registration. A single name is any underlying but SPY and VIX. At each partial sale its price is kept on the position as the floor from that day, and a later read says whether the remainder closed below it. The floor is wrong if the contracts left after the last partial sale closed for at least what that sale's price would have paid, zero at expiry, summed in the base currency over the positions; or if selling the remainder on the first day its statement value is below the floor kept that day does no better, in the same sum, than the mean of selling it at the statement value of a random held day (1,000 draws). Read once, at 40 positions. No order behaviour changes.",
		},
		{
			ID:    "option_sales_after_adverse_move",
			Title: "Option sales after a move against the position",
			Rule:  "One reading per name and week in which a long option was sold, in part or in full; VIX options are left out. Each sale is sorted by the underlying's move over the five US sessions ending with the close before the sale day, in units of its expected five-session move: the standard deviation of its daily log returns over the 252 sessions ending with that close, times the square root of five. Against the position by half an expected move or more (down for a call, up for a put), in its favour by half or more, or neither. Two groups are read, fixed now: after a move against, and after a move in favour. The reading is the contract's close five US sessions after the sale day against the price taken, in percent of the price taken, averaged over the week's sales in the group. Counted only where a closing quote is stored for that day, and the figure says it is a quote; this leaves out anything within five sessions of expiry. Sales after a move against the position count as too early only if that group's average is above zero in both the earlier and the later half of its readings by date, its 95% interval, resampled over calendar weeks, leaves out zero, and it is above the other group's average. Read once, at 50 readings in the group after a move against. The sample is sales made after registration; earlier sales are hindsight and stay out. No order behaviour changes.",
		},
	}
}
