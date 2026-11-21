package edge

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// The forward-test reader counts each registered test. It turns Edge's
// changes into the test's units (a name in a week, a contract in a week, or
// a position, as the test's rule says), with a group, a date and whether
// the unit's result is known yet, and tallies them against the number the
// test needs. It does not answer a test: the statistic that turns a full
// count into an answer is not built, so a test whose count is complete is
// full, never answered. Nothing here changes a verdict, a grade or an order.

// Decisions a test helps with.
const (
	ForwardInformsBuy  = "buy"
	ForwardInformsHold = "hold"
	ForwardInformsSell = "sell"
)

// Units a test counts.
const (
	ForwardUnitNameWeek        = "name_week"
	ForwardUnitContractWeek    = "contract_week"
	ForwardUnitPosition        = "position"
	ForwardUnitPartialPosition = "position_with_partial_sale"
)

// States of a test's count. Answered is reserved for the answers this build
// does not work out.
const (
	ForwardStateCollecting = "collecting"
	ForwardStateFull       = "full"
	ForwardStateAnswered   = "answered"
)

// Group ids that are not a test's own: the one group of a test without
// groups; units whose group fact Canary does not work out yet; and units
// that cannot be sorted because no record of their fact exists.
const (
	ForwardGroupAll        = "all"
	ForwardGroupNotSorted  = "not_sorted"
	ForwardGroupCannotSort = "cannot_sort"
	// ForwardNeedsAnyGroup is a number needed by either group on its own.
	ForwardNeedsAnyGroup = "any"
)

// States of one unit: its result is known (final), not known yet
// (waiting), or it gives none (excluded).
const (
	forwardUnitFinal    = "final"
	forwardUnitWaiting  = "waiting"
	forwardUnitExcluded = "excluded"
)

// Why a unit gives no result.
const (
	ForwardExcludedDelivered       = "closed_by_delivery"
	ForwardExcludedNotProved       = "position_not_proved"
	ForwardExcludedNoClose         = "no_close_stored"
	ForwardExcludedNoClosingQuote  = "no_closing_quote"
	ForwardExcludedSoldAtCost      = "sold_at_cost"
	ForwardExcludedUnderlyingUnset = "underlying_not_known"
	// ForwardExcludedSpread: the option was traded as a leg of a spread, so
	// judged alone it would misread the trade. Canary does not yet judge a
	// spread's legs as one trade.
	ForwardExcludedSpread = "part_of_spread"
)

// Why a test has no expected date.
const (
	ForwardNoDateNotWorkedOut = "group_not_worked_out"
	ForwardNoDateNoPace       = "no_pace"
	// ForwardNoDateNotRecorded: the trades before registration that would
	// set the pace have no record of the fact that sorts them.
	ForwardNoDateNotRecorded = "not_recorded_before"
	// ForwardNoDateNoneInGroup: units have results since registration, none
	// of them in the group the test needs.
	ForwardNoDateNoneInGroup = "none_in_needed_group"
)

// Why a test cannot fill in this version.
const (
	ForwardCannotFillNotWorkedOut = "group_not_worked_out"
	ForwardCannotFillBeyondWindow = "beyond_window"
)

// Bases of an expected date.
const (
	ForwardPaceSinceRegistration = "since_registration"
	ForwardPacePastYear          = "past_year"
)

// ForwardReviewDays is the review window units are counted from. Units are
// not stored apart from it yet, so a test that needs longer than this cannot
// fill.
const ForwardReviewDays = 365

// ForwardGroup is one group a test compares.
type ForwardGroup struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ForwardFacts says in plain words what a test is for and what it counts.
// It sits beside the registered rule and outside its hash: rewording a fact
// does not register the test again.
type ForwardFacts struct {
	Question string   `json:"question"`
	Informs  []string `json:"informs"`
	// Condition is the fact that can be checked at the moment of a trade
	// and that the test conditions on; empty when every trade counts.
	Condition string `json:"condition"`
	Unit      string `json:"unit"`
	// UnitText completes "counts ...".
	UnitText string `json:"unit_text"`
	Needs    int    `json:"needs"`
	// NeedsGroup is the group the number needed counts: ForwardGroupAll,
	// one group's id, or ForwardNeedsAnyGroup.
	NeedsGroup string `json:"needs_group"`
	// NeedsDays is the number of separate days the needed group must span.
	NeedsDays int            `json:"needs_days,omitempty"`
	Groups    []ForwardGroup `json:"groups"`
	// SampleLabel names the units of a test without groups.
	SampleLabel string `json:"sample_label"`
	// Policy names the written policy the test asks about, if any.
	Policy string `json:"policy,omitempty"`
	// AboutSales is set for the tests about sales. Every sale counts,
	// whoever sent it; the count also says how many of its units Canary's
	// protection sent, as information.
	AboutSales bool `json:"about_sales"`
	// NotSorted says what this version does not work out to sort the
	// units; empty when it can sort them.
	NotSorted string `json:"not_sorted,omitempty"`
	// CannotSort says why a unit has no record to sort it by.
	CannotSort string `json:"cannot_sort,omitempty"`
	// PaceNoun names one trade of the kind that sets the pace.
	PaceNoun string `json:"pace_noun"`
	// InShort is the test in plain words.
	InShort string `json:"in_short"`
}

// ForwardTestFacts returns the facts of a registered test by id.
func ForwardTestFacts(id string) (ForwardFacts, bool) {
	facts, ok := forwardFacts[id]
	if !ok {
		return ForwardFacts{}, false
	}
	facts.Informs = slices.Clone(facts.Informs)
	facts.Groups = slices.Clone(facts.Groups)
	return facts, true
}

var forwardFacts = map[string]ForwardFacts{
	"stock_sales_vs_nearby_day": {
		Question: "After you sell a stock, does it do worse than it does after a later day?",
		Informs:  []string{ForwardInformsSell}, Unit: ForwardUnitNameWeek, UnitText: "each stock or ETF once per week with a sale",
		Needs: 80, NeedsGroup: ForwardGroupAll, SampleLabel: "sales", AboutSales: true, PaceNoun: "stock sale",
		InShort: "Each stock or ETF you sell counts once per week. Canary looks at how the name moved over the five sessions after your sale and compares that with the same sale placed on a random later day, 2,000 times. The test holds if the name did worse after your sales than in nine out of ten of those runs, in the earlier and in the later half of the sample. It is answered once, at 80.",
	},
	"entries_vs_nearby_day": {
		Question: "Do your entries do better than the same entry on a random nearby day?",
		Informs:  []string{ForwardInformsBuy}, Unit: ForwardUnitNameWeek, UnitText: "each name once per week with an entry",
		Needs: 167, NeedsGroup: ForwardGroupAll, SampleLabel: "entries", PaceNoun: "entry",
		InShort: "Each name you buy or add to counts once per week. Canary looks at the underlying's move over the next five sessions, with the market's own move taken out, and compares it with the same entry placed on a random day within 20 sessions either side, 2,000 times. The test holds if your entries did better than nine out of ten of those runs, in both halves. It is answered once, at 167. An option traded as part of a spread is left out.",
	},
	"option_buys_below_premium_line": {
		Question:  "Do option buys made while premium at risk was at or below the budget line pay better than buys made above it?",
		Informs:   []string{ForwardInformsBuy},
		Condition: "premium at risk at the last statement against the Rulebook's premium budget line",
		Unit:      ForwardUnitContractWeek, UnitText: "each contract once per week with a buy, when its position has closed",
		Needs: 30, NeedsGroup: "at_or_below",
		Groups: []ForwardGroup{{ID: "at_or_below", Label: "at or below the line"}, {ID: "above", Label: "above the line"}},
		Policy: "the Rulebook's premium budget", PaceNoun: "option buy",
		NotSorted: "Canary cannot sort these buys yet: this version does not work out premium at risk at the last statement. Until it does, no count can grow.",
		InShort:   "Each option contract you buy counts once per week, once its position has closed. Its result is the average price you later sold at (zero if it expired), less the price you paid, after the buy's costs. Buys made while premium at risk was at or below the Rulebook's premium budget line are compared with buys made above it, by return on the premium paid. The line is not supported if the buys at or below it return no more. It is answered once, at 30 buys at or below the line. An option traded as part of a spread is left out.",
	},
	"option_buys_after_regime_warning": {
		Question:  "Do single-name option buys made the session after a Regime warning do better or worse than the rest?",
		Informs:   []string{ForwardInformsBuy},
		Condition: "Regime's stage at the last close",
		Unit:      ForwardUnitContractWeek, UnitText: "each contract once per week with a buy, when its position has closed",
		Needs: 40, NeedsGroup: "after_warning", NeedsDays: 20,
		Groups:     []ForwardGroup{{ID: "after_warning", Label: "after a warning"}, {ID: "other", Label: "after any other session"}},
		CannotSort: "Canary has no Regime record for the close before them", PaceNoun: "single-name option buy",
		InShort: "Each single-name option contract you buy counts once per week, once its position has closed. Its result is the average price you later sold at (zero if it expired), less the price you paid, after the buy's costs. Buys made the session after a Regime warning (early warning, confirmed stress or panic) are compared with all other such buys. No pass mark is set: the answer is a direction, looked at in the earlier and the later half of the buys. It is answered once, at 40 buys after a warning that fall on at least 20 separate days. An option traded as part of a spread is left out.",
	},
	"final_option_sales_vs_close_5_sessions": {
		Question:  "After you close an option position, is the contract worth more or less five sessions later?",
		Informs:   []string{ForwardInformsHold, ForwardInformsSell},
		Condition: "the sale price against the position's average cost",
		Unit:      ForwardUnitPosition, UnitText: "each closed single-name option position once",
		Needs: 30, NeedsGroup: ForwardNeedsAnyGroup, AboutSales: true,
		Groups:     []ForwardGroup{{ID: "above_cost", Label: "sold above cost"}, {ID: "below_cost", Label: "sold below cost"}},
		CannotSort: "the sale price or the position's average cost is missing", PaceNoun: "closing option sale",
		InShort: "Each single-name option position you close by a sale counts once. Canary compares the contract's closing quote five sessions later with the price you took. Positions sold above their average cost and positions sold below it are looked at separately, and neither is answered before it holds 30. It counts as a finding only if a group's average move points the same way in the earlier and the later half and its 95% confidence interval leaves out zero. A position traded as part of a spread is left out.",
	},
	"floor_after_partial_sale": {
		Question:  "After a partial sale, would a floor at that sale's price have protected what was left?",
		Informs:   []string{ForwardInformsSell},
		Condition: "the remainder's statement value against the price of the last partial sale",
		Unit:      ForwardUnitPartialPosition, UnitText: "each single-name option position with a partial sale once, when it has closed",
		Needs: 40, NeedsGroup: ForwardGroupAll, SampleLabel: "positions", AboutSales: true, PaceNoun: "partial sale",
		InShort: "Each single-name option position with a partial sale counts once, when it has closed. The price of the last partial sale is treated as a floor under what was left. The floor is wrong if the rest closed for at least that price anyway, or if selling the rest on the first day its value fell below the floor did no better than selling it on a random day. It is answered once, at 40 positions. A position traded as part of a spread is left out.",
	},
	"option_sales_after_adverse_move": {
		Question:  "After the underlying moved against you, is an option you sell worth more or less five sessions later?",
		Informs:   []string{ForwardInformsHold, ForwardInformsSell},
		Condition: "the underlying's move over the last five sessions, in expected moves",
		Unit:      ForwardUnitNameWeek, UnitText: "each name once per week with an option sale",
		Needs: 50, NeedsGroup: "against", AboutSales: true,
		Groups:    []ForwardGroup{{ID: "against", Label: "after a move against"}, {ID: "in_favour", Label: "after a move in favour"}},
		PaceNoun:  "option sale",
		NotSorted: "Canary cannot sort these sales yet: this version does not work out the underlying's five-session move in expected moves. Until it does, no count can grow.",
		InShort:   "Each name whose option you sell counts once per week. Sales are sorted by what the underlying did over the five sessions before: against your position by half an expected move or more, in its favour by as much, or neither. Canary compares the contract's closing quote five sessions after the sale with the price you took. Selling after a move against you counts as too early only if those contracts were worth more on average in both halves, with a 95% confidence interval that leaves out zero, and by more than after a move in your favour. It is answered once, at 50. An option traded as part of a spread is left out.",
	},
}

// ForwardCountInput is what the counter reads. The two functions come from
// the daemon: the official US calendar and the record of each US close.
type ForwardCountInput struct {
	Changes []Change
	// After and Through bound the decisions counted: strictly after After
	// (the registration instant) and, when set, at or before Through.
	After, Through time.Time
	// AsOf is the moment of the evidence: a week, a position or five
	// sessions are over only by then.
	AsOf time.Time
	// ProtectionKnown is false when protection evidence was withheld from
	// the reply. Sales count either way; only which of them Canary's
	// protection sent is then not marked.
	ProtectionKnown bool
	// Zone is New York; weeks and days are its dates.
	Zone *time.Location
	// FifthSessionClose is the close of the fifth US session after the
	// session of a decision made at the given instant.
	FifthSessionClose func(time.Time) (time.Time, bool)
	// PriorSessionStage is Regime's stage recorded at the close of the US
	// session before the decision's New York date.
	PriorSessionStage func(time.Time) (string, bool)
	// spread holds the ids of the changes that are a spread leg, worked out
	// once from all Changes.
	spread map[string]bool
}

// ForwardUnit is one counted unit of a test. Label names it for a reader:
// the name of a name-week, the contract of a contract-week or position.
// Protection marks a unit of a test about sales that Canary's protection
// sent, when that is known; it is information and changes no count.
type ForwardUnit struct {
	Key        string
	Label      string
	Group      string
	Protection bool
	Date       time.Time
	State      string
	Reason     string
}

// CountForwardUnits turns changes into the units of the rule with this id.
func CountForwardUnits(id string, in ForwardCountInput) []ForwardUnit {
	if in.Zone == nil {
		in.Zone = time.UTC
	}
	var changes []Change
	for _, change := range in.Changes {
		if !InForwardSample(in.After, change.ExecutedAt) || !in.Through.IsZero() && change.ExecutedAt.After(in.Through) {
			continue
		}
		changes = append(changes, change)
	}
	slices.SortStableFunc(changes, func(a, b Change) int { return a.ExecutedAt.Compare(b.ExecutedAt) })
	in.spread = spreadLegs(in.Changes)
	switch id {
	case "stock_sales_vs_nearby_day":
		return in.countWeeks(changes, func(c Change) bool { return stockLike(c) && longSale(c) }, ForwardGroupAll, in.stockSaleState)
	case "entries_vs_nearby_day":
		units := in.countWeeks(changes, func(c Change) bool {
			return longEntry(c) && (stockLike(c) || isOption(c) && singleName(c))
		}, ForwardGroupAll, in.notSpread(in.entryState))
		return append(units, in.unnamed(changes, longEntry)...)
	case "option_buys_below_premium_line":
		return in.countOptionBuys(changes, func(Change) bool { return true }, func(Change) string { return ForwardGroupNotSorted })
	case "option_buys_after_regime_warning":
		units := in.countOptionBuys(changes, singleName, in.regimeGroup)
		return append(units, in.unnamed(changes, longEntry)...)
	case "final_option_sales_vs_close_5_sessions":
		units := in.countFinalSales(changes)
		return append(units, in.unnamed(changes, func(c Change) bool { return c.Direction == DirectionLong && c.Action == ActionExit })...)
	case "floor_after_partial_sale":
		units := in.countPartialSales(changes)
		return append(units, in.unnamed(changes, func(c Change) bool { return c.Direction == DirectionLong && c.Action == ActionTrim })...)
	case "option_sales_after_adverse_move":
		units := in.countWeeks(changes, func(c Change) bool {
			return isOption(c) && longSale(c) && c.Underlying != "" && !strings.EqualFold(c.Underlying, "VIX")
		}, ForwardGroupNotSorted, in.notSpread(in.optionSaleState))
		return append(units, in.unnamed(changes, longSale)...)
	}
	return nil
}

// unnamed gives one no-result unit per option decision of the test's kind
// whose statement named no underlying: whether it is a single name, and
// which name it is, cannot be told, so it is said rather than dropped.
func (in ForwardCountInput) unnamed(changes []Change, kind func(Change) bool) []ForwardUnit {
	var out []ForwardUnit
	for _, c := range changes {
		if isOption(c) && strings.TrimSpace(c.Underlying) == "" && kind(c) {
			out = append(out, ForwardUnit{Key: c.ID, Label: c.Symbol, Group: ForwardGroupCannotSort, Date: in.day(c.ExecutedAt), State: forwardUnitExcluded, Reason: ForwardExcludedUnderlyingUnset})
		}
	}
	return out
}

// spreadLegs names the option changes that are a spread leg: their order
// also traded another option contract on the same underlying. The order is
// the one the statement links the executions to (Change.Order); nothing is
// inferred from times, strikes or symbols, so a spread built from separate
// orders is not recognised. It reads every change given, inside the sample
// or not, so a leg whose other leg falls outside the sample is still a leg.
func spreadLegs(changes []Change) map[string]bool {
	type order struct{ key, underlying string }
	key := func(c Change) (order, bool) {
		u := strings.ToUpper(strings.TrimSpace(c.Underlying))
		return order{c.Order, u}, isOption(c) && c.Order != "" && u != ""
	}
	contracts := map[order]map[string]bool{}
	for _, c := range changes {
		if k, ok := key(c); ok {
			if contracts[k] == nil {
				contracts[k] = map[string]bool{}
			}
			contracts[k][strings.ToUpper(strings.TrimSpace(c.Symbol))] = true
		}
	}
	out := map[string]bool{}
	for _, c := range changes {
		if k, ok := key(c); ok && len(contracts[k]) > 1 {
			out[c.ID] = true
		}
	}
	return out
}

// notSpread leaves a spread leg out of its unit, as a decision without a
// result is; the other decisions keep their state.
func (in ForwardCountInput) notSpread(state func(Change) (string, string)) func(Change) (string, string) {
	return func(c Change) (string, string) {
		if in.spread[c.ID] {
			return forwardUnitExcluded, ForwardExcludedSpread
		}
		return state(c)
	}
}

// spreadPositions names the option positions with a trade, among all the
// changes given, that is a spread leg.
func (in ForwardCountInput) spreadPositions() map[string]bool {
	out := map[string]bool{}
	for _, c := range in.Changes {
		if in.spread[c.ID] && c.Position != nil && c.Position.PositionID != "" {
			out[c.Position.PositionID] = true
		}
	}
	return out
}

// positionSpread says whether a position unit is left out as part of a
// spread: a trade of its position is a spread leg, or, for a change Edge
// could tie to no position, the change itself is.
func (in ForwardCountInput) positionSpread(c Change, positions map[string]bool) bool {
	if c.Position != nil && c.Position.PositionID != "" && positions[c.Position.PositionID] {
		return true
	}
	return in.spread[c.ID]
}

func stockLike(c Change) bool {
	asset := strings.ToUpper(c.AssetClass)
	return asset == "STK" || asset == "ETF"
}

func isOption(c Change) bool { return strings.EqualFold(c.AssetClass, "OPT") }

func longSale(c Change) bool {
	return c.Direction == DirectionLong && (c.Action == ActionTrim || c.Action == ActionExit)
}

func longEntry(c Change) bool {
	return c.Direction == DirectionLong && (c.Action == ActionOpen || c.Action == ActionAdd)
}

// singleName is the rules' "any underlying but SPY and VIX". An option
// whose underlying the statement did not name is not a single name.
func singleName(c Change) bool {
	u := strings.ToUpper(strings.TrimSpace(c.Underlying))
	return u != "" && u != "SPY" && u != "VIX"
}

// name is what a name-week is keyed on: the underlying of an option, the
// symbol of a stock or ETF.
func name(c Change) string {
	if isOption(c) {
		return strings.ToUpper(strings.TrimSpace(c.Underlying))
	}
	return strings.ToUpper(strings.TrimSpace(c.Symbol))
}

func (in ForwardCountInput) day(t time.Time) time.Time {
	local := t.In(in.Zone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, in.Zone)
}

// week is the ISO week of the decision's New York date, as the scorecard's
// name-week is, and the instant it ends.
func (in ForwardCountInput) week(t time.Time) (string, time.Time) {
	day := in.day(t)
	year, week := day.ISOWeek()
	offset := (int(day.Weekday()) + 6) % 7
	end := day.AddDate(0, 0, 7-offset)
	return fmt.Sprintf("%04d-W%02d", year, week), end
}

// sentByProtection says whether Canary's protection sent the sale, when
// the reply serves that evidence.
func (in ForwardCountInput) sentByProtection(c Change) bool {
	return in.ProtectionKnown && c.ProtectionContext.MatchedExecutions > 0
}

// fiveSessionsOver says whether the fifth US session after the decision has
// closed by the evidence's moment.
func (in ForwardCountInput) fiveSessionsOver(c Change) bool {
	if in.FifthSessionClose == nil {
		return false
	}
	at, ok := in.FifthSessionClose(c.ExecutedAt)
	return ok && !in.AsOf.Before(at)
}

func fiveSessionScore(c Change) (HorizonScore, bool) {
	for _, score := range c.Scores {
		if score.Sessions == 5 {
			return score, score.HorizonClose != nil
		}
	}
	return HorizonScore{}, false
}

// stockSaleState: the name's close five sessions after each sale.
func (in ForwardCountInput) stockSaleState(c Change) (string, string) {
	if !in.fiveSessionsOver(c) {
		return forwardUnitWaiting, ""
	}
	if _, ok := fiveSessionScore(c); !ok {
		return forwardUnitExcluded, ForwardExcludedNoClose
	}
	return forwardUnitFinal, ""
}

// entryState: five sessions after each entry. The underlying's own close is
// not checked: Edge does not fetch an option's underlying.
func (in ForwardCountInput) entryState(c Change) (string, string) {
	if !in.fiveSessionsOver(c) {
		return forwardUnitWaiting, ""
	}
	return forwardUnitFinal, ""
}

// optionSaleState: a closing quote stored for the fifth session, as the rule
// counts only those; a statement value of a contract still held is not one.
func (in ForwardCountInput) optionSaleState(c Change) (string, string) {
	if !in.fiveSessionsOver(c) {
		return forwardUnitWaiting, ""
	}
	if score, ok := fiveSessionScore(c); !ok || score.CloseSource != CloseSourceClosingQuote {
		return forwardUnitExcluded, ForwardExcludedNoClosingQuote
	}
	return forwardUnitFinal, ""
}

// countWeeks counts name-weeks. A week is final once it is over and each of
// its decisions is; a decision that can give no reading is left out of the
// week, and a week left with none gives no reading. A week is marked as
// Canary's protection's when it sent at least one sale the week counts.
func (in ForwardCountInput) countWeeks(changes []Change, include func(Change) bool, group string, state func(Change) (string, string)) []ForwardUnit {
	type acc struct {
		unit             ForwardUnit
		end              time.Time
		waiting, results int
		reason           string
	}
	var order []string
	units := map[string]*acc{}
	for _, c := range changes {
		if !include(c) {
			continue
		}
		week, end := in.week(c.ExecutedAt)
		key := name(c) + ":" + week
		a := units[key]
		if a == nil {
			a = &acc{unit: ForwardUnit{Key: key, Label: name(c), Group: group, Date: in.day(c.ExecutedAt)}, end: end}
			units[key] = a
			order = append(order, key)
		}
		st, reason := state(c)
		switch st {
		case forwardUnitWaiting:
			a.waiting++
		case forwardUnitFinal:
			a.results++
		default:
			a.reason = reason
		}
		if st != forwardUnitExcluded && longSale(c) && in.sentByProtection(c) {
			a.unit.Protection = true
		}
	}
	out := make([]ForwardUnit, 0, len(order))
	for _, key := range order {
		a := units[key]
		switch {
		case a.waiting > 0 || in.AsOf.Before(a.end):
			a.unit.State = forwardUnitWaiting
		case a.results > 0:
			a.unit.State = forwardUnitFinal
		default:
			a.unit.State, a.unit.Reason = forwardUnitExcluded, a.reason
		}
		out = append(out, a.unit)
	}
	return out
}

// positionState is final once the position has closed. A position closed by
// delivery gives no reading, and one Edge could not prove has no identity.
func positionState(c Change, deliveryGivesNone bool) (string, string) {
	p := c.Position
	if p == nil || p.PositionID == "" || p.State == OptionPositionUnproved {
		return forwardUnitExcluded, ForwardExcludedNotProved
	}
	if p.State != OptionPositionClosed {
		return forwardUnitWaiting, ""
	}
	if deliveryGivesNone && (p.ClosedBy == "exercise" || p.ClosedBy == "assignment") {
		return forwardUnitExcluded, ForwardExcludedDelivered
	}
	return forwardUnitFinal, ""
}

// countOptionBuys counts contract-weeks of option buys that open or add to
// a long position, one per position and week. A buy that is a spread leg is
// left out of its contract-week; a contract-week with no other buy gives no
// reading, by that reason.
func (in ForwardCountInput) countOptionBuys(changes []Change, include func(Change) bool, group func(Change) string) []ForwardUnit {
	var out []ForwardUnit
	seen := map[string]bool{}
	spreadOnly := map[string]int{}
	for _, c := range changes {
		if !isOption(c) || !longEntry(c) || !include(c) {
			continue
		}
		week, _ := in.week(c.ExecutedAt)
		position := ""
		if c.Position != nil {
			position = c.Position.PositionID
		}
		key := position + ":" + week
		if position == "" {
			key = c.ID
		}
		if seen[key] {
			continue
		}
		if in.spread[c.ID] {
			if _, ok := spreadOnly[key]; !ok {
				spreadOnly[key] = len(out)
				out = append(out, ForwardUnit{Key: key, Label: c.Symbol, Group: group(c), Date: in.day(c.ExecutedAt), State: forwardUnitExcluded, Reason: ForwardExcludedSpread})
			}
			continue
		}
		seen[key] = true
		st, reason := positionState(c, true)
		unit := ForwardUnit{Key: key, Label: c.Symbol, Group: group(c), Date: in.day(c.ExecutedAt), State: st, Reason: reason}
		if i, ok := spreadOnly[key]; ok {
			out[i] = unit
			continue
		}
		out = append(out, unit)
	}
	return out
}

// regimeGroup labels a buy by Regime's stage of the session before it; a
// session without a record is not read, never given a neighbour's stage.
func (in ForwardCountInput) regimeGroup(c Change) string {
	if in.PriorSessionStage == nil {
		return ForwardGroupCannotSort
	}
	stage, ok := in.PriorSessionStage(c.ExecutedAt)
	if !ok {
		return ForwardGroupCannotSort
	}
	return ForwardRegimeGroup(stage)
}

// ForwardRegimeGroup is the group of option_buys_after_regime_warning a buy
// after a session with this Regime stage joins: warning is early warning,
// confirmed stress or panic; the rest is quiet, stabilization or
// opportunity; any other stage cannot be sorted.
func ForwardRegimeGroup(stage string) string {
	switch stage {
	case "early_warning", "confirmed_stress", "panic":
		return "after_warning"
	case "quiet", "stabilization", "opportunity":
		return "other"
	}
	return ForwardGroupCannotSort
}

// countFinalSales counts closed single-name long option positions by the
// sale that closed them. An expiry is not a sale and is not a change.
func (in ForwardCountInput) countFinalSales(changes []Change) []ForwardUnit {
	var out []ForwardUnit
	spread := in.spreadPositions()
	for _, c := range changes {
		if !isOption(c) || c.Direction != DirectionLong || c.Action != ActionExit || !singleName(c) {
			continue
		}
		unit := ForwardUnit{Key: c.ID, Label: c.Symbol, Group: ForwardGroupCannotSort, Protection: in.sentByProtection(c), Date: in.day(c.ExecutedAt)}
		if in.positionSpread(c, spread) {
			if c.Position != nil && c.Position.PositionID != "" {
				unit.Key = c.Position.PositionID
			}
			unit.State, unit.Reason = forwardUnitExcluded, ForwardExcludedSpread
			out = append(out, unit)
			continue
		}
		if c.Position == nil || c.Position.PositionID == "" {
			unit.State, unit.Reason = forwardUnitExcluded, ForwardExcludedNotProved
			out = append(out, unit)
			continue
		}
		unit.Key = c.Position.PositionID
		if p := c.Position; p.ReferenceKind == OptionReferenceEntryAverage && p.ResultPct != nil {
			switch {
			case *p.ResultPct > 0:
				unit.Group = "above_cost"
			case *p.ResultPct < 0:
				unit.Group = "below_cost"
			}
		}
		unit.State, unit.Reason = in.optionSaleState(c)
		if unit.State != forwardUnitExcluded && unit.Group == ForwardGroupCannotSort && c.Position.ResultPct != nil && *c.Position.ResultPct == 0 {
			unit.State, unit.Reason = forwardUnitExcluded, ForwardExcludedSoldAtCost
		}
		out = append(out, unit)
	}
	return out
}

// countPartialSales counts single-name long option positions with a partial
// sale in the sample. A position on which Canary's protection sent any sale,
// in the window, is marked as such.
func (in ForwardCountInput) countPartialSales(sampled []Change) []ForwardUnit {
	spread := in.spreadPositions()
	protected := map[string]bool{}
	for _, c := range in.Changes {
		if isOption(c) && longSale(c) && c.Position != nil && c.Position.PositionID != "" && in.sentByProtection(c) {
			protected[c.Position.PositionID] = true
		}
	}
	var out []ForwardUnit
	seen := map[string]bool{}
	for _, c := range sampled {
		if !isOption(c) || c.Direction != DirectionLong || c.Action != ActionTrim || !singleName(c) {
			continue
		}
		unit := ForwardUnit{Key: c.ID, Label: c.Symbol, Group: ForwardGroupAll, Date: in.day(c.ExecutedAt)}
		if c.Position != nil && c.Position.PositionID != "" {
			if seen[c.Position.PositionID] {
				continue
			}
			seen[c.Position.PositionID] = true
			unit.Key = c.Position.PositionID
		}
		unit.Protection = protected[unit.Key] || in.sentByProtection(c)
		unit.State, unit.Reason = positionState(c, false)
		if in.positionSpread(c, spread) {
			unit.State, unit.Reason = forwardUnitExcluded, ForwardExcludedSpread
		}
		out = append(out, unit)
	}
	return out
}

// ForwardCount is units with a result and units waiting for one.
type ForwardCount struct {
	Final   int `json:"final"`
	Waiting int `json:"waiting"`
}

// ForwardGroupCount is one group's tally in the test sample.
type ForwardGroupCount struct {
	ID      string `json:"id"`
	Final   int    `json:"final"`
	Waiting int    `json:"waiting"`
	// Days is the number of separate days the group's final units fall on.
	Days int `json:"days"`
}

// ForwardMonth is one month's final units that count toward the number
// needed, by unit date.
type ForwardMonth struct {
	Month string `json:"month"`
	Final int    `json:"final"`
}

// ForwardExpected is when the count is expected to be full.
type ForwardExpected struct {
	Date  time.Time `json:"date"`
	Basis string    `json:"basis"`
	// BeyondWindow is set when the date lies more than the review window
	// after registration: by then the first units have left the window, so
	// the test cannot fill until counted units are stored.
	BeyondWindow bool `json:"beyond_window"`
}

// ForwardUnitRef names one unit for a reader.
type ForwardUnitRef struct {
	Key    string    `json:"key"`
	Label  string    `json:"label"`
	Date   time.Time `json:"date"`
	Group  string    `json:"group,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// ForwardSample is the tally of one test.
type ForwardSample struct {
	State string `json:"state"`
	// Test is the sample the test is answered on; Groups splits it.
	Test   ForwardCount        `json:"test"`
	Groups []ForwardGroupCount `json:"groups"`
	// Protection, for the tests about sales, is how many of the counted
	// units Canary's protection sent. It is information: those units are in
	// Test like any other.
	Protection *ForwardCount    `json:"protection,omitempty"`
	Excluded   map[string]int   `json:"excluded"`
	FirstUnit  time.Time        `json:"first_unit,omitzero"`
	LastUnit   time.Time        `json:"last_unit,omitzero"`
	Months     []ForwardMonth   `json:"months"`
	ToFill     int              `json:"to_fill"`
	Expected   *ForwardExpected `json:"expected,omitempty"`
	NoDate     string           `json:"no_date,omitempty"`
	CanFill    bool             `json:"can_fill"`
	CannotFill string           `json:"cannot_fill,omitempty"`
	// WaitingUnits and NotCounted name the units of the test sample still
	// waiting for a result and the units that give none.
	WaitingUnits []ForwardUnitRef `json:"waiting_units"`
	NotCounted   []ForwardUnitRef `json:"not_counted"`
}

// SummarizeForward tallies a test's units since registration and dates a
// full count. paceUnits are the same kind of units from the review window
// before registration: they set a pace while fewer than three units since
// registration have a result, and never enter the sample.
func SummarizeForward(facts ForwardFacts, units, paceUnits []ForwardUnit, registeredAt, asOf time.Time, zone *time.Location) ForwardSample {
	if zone == nil {
		zone = time.UTC
	}
	out := ForwardSample{State: ForwardStateCollecting, Excluded: map[string]int{}, Months: []ForwardMonth{}, CanFill: true, WaitingUnits: []ForwardUnitRef{}, NotCounted: []ForwardUnitRef{}}
	groups := map[string]*ForwardGroupCount{}
	ids := []string{ForwardGroupAll}
	if len(facts.Groups) > 0 {
		ids = ids[:0]
		for _, g := range facts.Groups {
			ids = append(ids, g.ID)
		}
	}
	if facts.NotSorted != "" {
		ids = append(ids, ForwardGroupNotSorted)
	}
	if len(facts.Groups) > 0 && slices.ContainsFunc(units, func(u ForwardUnit) bool { return u.Group == ForwardGroupCannotSort && u.State != forwardUnitExcluded }) {
		ids = append(ids, ForwardGroupCannotSort)
	}
	for _, id := range ids {
		out.Groups = append(out.Groups, ForwardGroupCount{ID: id})
	}
	for i := range out.Groups {
		groups[out.Groups[i].ID] = &out.Groups[i]
	}
	if facts.AboutSales {
		out.Protection = &ForwardCount{}
	}
	days := map[string]map[string]bool{}
	months := map[string]int{}
	for _, u := range units {
		ref := ForwardUnitRef{Key: u.Key, Label: u.Label, Date: u.Date, Group: u.Group}
		if u.State == forwardUnitExcluded {
			out.Excluded[u.Reason]++
			ref.Reason = u.Reason
			out.NotCounted = append(out.NotCounted, ref)
			continue
		}
		tally(&out.Test, u)
		if u.Protection && out.Protection != nil {
			tally(out.Protection, u)
		}
		if g := groups[u.Group]; g != nil {
			if u.State == forwardUnitFinal {
				g.Final++
				if days[u.Group] == nil {
					days[u.Group] = map[string]bool{}
				}
				days[u.Group][u.Date.Format(time.DateOnly)] = true
			} else {
				g.Waiting++
			}
		}
		if u.State == forwardUnitWaiting {
			out.WaitingUnits = append(out.WaitingUnits, ref)
		}
		if out.FirstUnit.IsZero() || u.Date.Before(out.FirstUnit) {
			out.FirstUnit = u.Date
		}
		if u.Date.After(out.LastUnit) {
			out.LastUnit = u.Date
		}
		if u.State == forwardUnitFinal && countsTowardNeeds(facts, u.Group) {
			months[u.Date.In(zone).Format("2006-01")]++
		}
	}
	for i := range out.Groups {
		out.Groups[i].Days = len(days[out.Groups[i].ID])
	}
	if !registeredAt.IsZero() && !asOf.Before(registeredAt) {
		start := time.Date(registeredAt.In(zone).Year(), registeredAt.In(zone).Month(), 1, 0, 0, 0, 0, zone)
		for month := start; !month.After(asOf.In(zone)); month = month.AddDate(0, 1, 0) {
			key := month.Format("2006-01")
			out.Months = append(out.Months, ForwardMonth{Month: key, Final: months[key]})
		}
	}
	have, met := 0, false
	for _, g := range out.Groups {
		if !countsTowardNeeds(facts, g.ID) {
			continue
		}
		have = max(have, g.Final)
		met = met || g.Final >= facts.Needs && g.Days >= facts.NeedsDays
	}
	out.ToFill = max(facts.Needs-have, 0)
	if met {
		out.State = ForwardStateFull
		return out
	}
	out.Expected, out.NoDate = forwardExpected(facts, out, paceUnits, registeredAt, asOf)
	switch {
	case facts.NotSorted != "":
		out.CanFill, out.CannotFill = false, ForwardCannotFillNotWorkedOut
	case out.Expected != nil && out.Expected.BeyondWindow:
		out.CanFill, out.CannotFill = false, ForwardCannotFillBeyondWindow
	}
	return out
}

// countsTowardNeeds says whether a group's units count toward the number
// the test needs.
func countsTowardNeeds(facts ForwardFacts, group string) bool {
	if group == ForwardGroupNotSorted || group == ForwardGroupCannotSort {
		return false
	}
	return facts.NeedsGroup == ForwardGroupAll || facts.NeedsGroup == ForwardNeedsAnyGroup || facts.NeedsGroup == group
}

func tally(c *ForwardCount, u ForwardUnit) {
	if u.State == forwardUnitFinal {
		c.Final++
	} else {
		c.Waiting++
	}
}

// forwardExpected dates a full count from the needed group's pace of units
// with a result: since registration once three have one, otherwise over the
// review window before registration.
func forwardExpected(facts ForwardFacts, sample ForwardSample, paceUnits []ForwardUnit, registeredAt, asOf time.Time) (*ForwardExpected, string) {
	if registeredAt.IsZero() {
		return nil, ForwardNoDateNoPace
	}
	have := map[string]int{}
	for _, g := range sample.Groups {
		if countsTowardNeeds(facts, g.ID) {
			have[g.ID] = g.Final
		}
	}
	basis, counts, days := ForwardPaceSinceRegistration, maps.Clone(have), asOf.Sub(registeredAt).Hours()/24
	if sample.Test.Final < 3 || days <= 0 {
		basis, days = ForwardPacePastYear, registeredAt.Sub(asOf.AddDate(0, 0, -ForwardReviewDays)).Hours()/24
		counts = map[string]int{}
		for _, u := range paceUnits {
			if u.State == forwardUnitFinal && countsTowardNeeds(facts, u.Group) {
				counts[u.Group]++
			}
		}
	}
	best := math.Inf(1)
	for id, n := range counts {
		if n <= 0 || days <= 0 {
			continue
		}
		best = min(best, float64(max(facts.Needs-have[id], 0))/(float64(n)/days))
	}
	if math.IsInf(best, 1) {
		switch {
		case facts.NotSorted != "":
			return nil, ForwardNoDateNotWorkedOut
		case basis == ForwardPaceSinceRegistration:
			return nil, ForwardNoDateNoneInGroup
		case slices.ContainsFunc(paceUnits, func(u ForwardUnit) bool { return u.Group == ForwardGroupCannotSort && u.State == forwardUnitFinal }):
			return nil, ForwardNoDateNotRecorded
		}
		return nil, ForwardNoDateNoPace
	}
	date := asOf.Add(time.Duration(best * 24 * float64(time.Hour)))
	return &ForwardExpected{Date: date, Basis: basis, BeyondWindow: date.After(registeredAt.AddDate(0, 0, ForwardReviewDays))}, ""
}
