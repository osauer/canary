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

// ForwardTests returns the registered rules in their fixed order. The
// wording was approved by the owner on 2026-11-20 (06:25 CET, and two
// sentences in rules 3 and 4 at 06:55 CET); it carries rules only and no
// result.
func ForwardTests() []ForwardTest {
	return []ForwardTest{
		{
			ID:    "stock_sales_vs_nearby_day",
			Title: "Stock sales against a nearby day",
			Rule:  "One reading per name and week in which a stock or ETF was sold: the five-session price impact of the sales, against the same sales placed on a random session within 20 sessions either side (2,000 draws). The reading holds if sales are followed by weaker five-session moves than nearby days in both halves of the sample. Read after about 80 sale name-weeks.",
		},
		{
			ID:    "entries_vs_nearby_day",
			Title: "Entries against a nearby day",
			Rule:  "One reading per name and week in which a position was entered: the signed five-session move of the underlying net of SPY after the entry, against a random nearby day. Holds if above the 90th percentile of the random days in both halves. Read at 167 to 328 name-weeks, pooled.",
		},
		{
			ID:    "option_buys_below_premium_line",
			Title: "Option buys below the premium line against above it",
			Rule:  "The position result of option buys made on days when premium at risk at the prior statement was below the line, against buys made above it, one reading per contract and week. The line is the Rulebook's premium budget act level (premium_budget_act_pct, percent of net liquidation) in the regime set the Rulebook applied that day, read from the policy file. Read after 30 buys below the line.",
		},
		{
			ID:    "option_buys_after_regime_warning",
			Title: "Single-name option buys after a Regime warning session",
			Rule:  "Against all other single-name option buys, on the position reading. The label is Regime's stage of the session before the buy: warning is early warning, confirmed stress or panic; the rest is quiet, stabilization or opportunity. No pass threshold is set for this test; it is read by direction in both halves of the sample.",
		},
		{
			ID:    "final_option_sales_vs_close_5_sessions",
			Title: "Final option sales against the close five sessions later",
			Rule:  "One reading per position, single names only: the contract's close five US sessions after the sale that closed the position, against the price taken, on the contracts of that sale. Two groups, fixed now: sold above the position's average cost, and sold below it. Counted only where a closing quote is stored for that day, and the figure says it is a quote; this leaves out anything within five sessions of expiry. No reading before 30 positions in a group. It counts as a rule only if one group's average move, in percent of the price taken, has the same sign in two halves of the sample and its interval, resampled over positions, leaves out zero. The sample is sales made after registration; a close fetched for an earlier sale is hindsight and stays out.",
		},
		{
			ID:    "floor_after_partial_sale",
			Title: "A floor under what is left after a partial sale (registered, not built)",
			Rule:  "At each partial sale of an option position its price is kept on the position, and a later read says whether the remainder closed below it. The floor is wrong if, over the next 40 to 55 positions with a partial sale, the remainder closes at or above the last partial sale's price on average, or if a floor recorded on the day does no better than selling on a random held day. No order behaviour changes.",
		},
	}
}
