package edge

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// The forward-test reader counts each registered rule's sample. It turns
// Edge's changes into the rule's units (key, group, date, and whether the
// unit is final as the rule's text defines it) and tallies them against the
// rule's read point. It does not read a rule: the statistic that turns a full
// sample into an answer is not built, so a rule whose read point is met is
// due, never read. Nothing here changes a verdict, a grade or an order.

// Decisions a rule informs.
const (
	ForwardInformsBuy  = "buy"
	ForwardInformsHold = "hold"
	ForwardInformsSell = "sell"
)

// Units a rule counts.
const (
	ForwardUnitNameWeek        = "name_week"
	ForwardUnitContractWeek    = "contract_week"
	ForwardUnitPosition        = "position"
	ForwardUnitPartialPosition = "position_with_partial_sale"
)

// States of a rule's sample. Read is reserved for the reading this build
// does not compute.
const (
	ForwardStateCollecting = "collecting"
	ForwardStateDue        = "due"
	ForwardStateRead       = "read"
)

// Group ids that are not a rule's own: the one group of a rule without
// groups, and units whose group fact is not read.
const (
	ForwardGroupAll     = "all"
	ForwardGroupNotRead = "not_read"
	// ForwardReadAnyGroup is a read point met by either group on its own.
	ForwardReadAnyGroup = "any"
)

// Samples a unit belongs to. A sale a protection rule sent is read apart,
// and a sale whose sender cannot be told this time counts in neither.
const (
	forwardSampleTest       = "test"
	forwardSampleProtection = "protection"
	forwardSampleUnknown    = "sender_unknown"
)

// States of one unit.
const (
	forwardUnitFinal    = "final"
	forwardUnitPending  = "pending"
	forwardUnitExcluded = "excluded"
)

// Why a unit gives no reading.
const (
	ForwardExcludedDelivered       = "closed_by_delivery"
	ForwardExcludedNotProved       = "position_not_proved"
	ForwardExcludedNoClose         = "no_close_stored"
	ForwardExcludedNoClosingQuote  = "no_closing_quote"
	ForwardExcludedSoldAtCost      = "sold_at_cost"
	ForwardExcludedUnderlyingUnset = "underlying_not_known"
)

// Why a rule has no expected date.
const (
	ForwardNoDateGroupNotRead = "group_not_read"
	ForwardNoDateNoPace       = "no_pace"
	ForwardNoDateSenderUnread = "sender_not_known"
	// ForwardNoDateNotRecorded: the decisions before registration that
	// would set the pace have no record of the fact that groups them.
	ForwardNoDateNotRecorded = "not_recorded_before"
	// ForwardNoDateNoneInGroup: decisions are final since registration,
	// none of them in the group the rule is read on.
	ForwardNoDateNoneInGroup = "none_in_read_group"
)

// Bases of an expected date.
const (
	ForwardPaceSinceRegistration = "since_registration"
	ForwardPacePastYear          = "past_year"
)

// ForwardReviewDays is the review window units are counted from. Units are
// not stored apart from it yet, so a rule that needs longer than this cannot
// fill.
const ForwardReviewDays = 365

// ForwardGroup is one group a rule compares.
type ForwardGroup struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ForwardFacts says in plain words what a rule is for and what it counts.
// It sits beside the registered text and outside its hash: rewording a fact
// does not register the rule again.
type ForwardFacts struct {
	Question string   `json:"question"`
	Informs  []string `json:"informs"`
	// Condition is the fact that can be checked at the moment of a decision
	// and that the rule conditions on; empty when every decision counts.
	Condition string `json:"condition"`
	Unit      string `json:"unit"`
	UnitText  string `json:"unit_text"`
	ReadAt    int    `json:"read_at"`
	// ReadGroup is the group the read point counts: ForwardGroupAll, one
	// group's id, or ForwardReadAnyGroup.
	ReadGroup string `json:"read_group"`
	// ReadDays is the number of separate days the read group must span.
	ReadDays int            `json:"read_days,omitempty"`
	Groups   []ForwardGroup `json:"groups"`
	// SampleLabel names the units of a rule without groups.
	SampleLabel string `json:"sample_label"`
	Policy      string `json:"policy,omitempty"`
	// ProtectionApart is set for the rules about sales: a sale a protection
	// rule sent is read apart and counts neither in the test nor toward the
	// read point.
	ProtectionApart bool `json:"protection_apart"`
	// GroupNotRead says why this build cannot tell the groups apart; empty
	// when it can.
	GroupNotRead string `json:"group_not_read,omitempty"`
}

// ForwardTestFacts returns the facts of a registered rule by id.
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
		Question: "Are your stock sales followed by weaker prices than a sale a few days later would have been?",
		Informs:  []string{ForwardInformsSell}, Unit: ForwardUnitNameWeek, UnitText: "one name in one week with a stock or ETF sale",
		ReadAt: 80, ReadGroup: ForwardGroupAll, SampleLabel: "sales you chose", ProtectionApart: true,
	},
	"entries_vs_nearby_day": {
		Question: "Do your entries do better than the same entry on a random nearby day?",
		Informs:  []string{ForwardInformsBuy}, Unit: ForwardUnitNameWeek, UnitText: "one name in one week with an entry",
		ReadAt: 167, ReadGroup: ForwardGroupAll, SampleLabel: "entries",
	},
	"option_buys_below_premium_line": {
		Question:  "Do option buys made while premium at risk was at or below the budget line pay better than buys made above it?",
		Informs:   []string{ForwardInformsBuy},
		Condition: "premium at risk at the last statement against the Rulebook's premium budget line",
		Unit:      ForwardUnitContractWeek, UnitText: "one contract in one week, final when its position has closed",
		ReadAt: 30, ReadGroup: "at_or_below",
		Groups: []ForwardGroup{{ID: "at_or_below", Label: "at or below the line"}, {ID: "above", Label: "above the line"}},
		Policy: "the Rulebook's premium budget", GroupNotRead: "premium at risk at the prior statement is not computed yet",
	},
	"option_buys_after_regime_warning": {
		Question:  "Do single-name option buys made the session after a Regime warning do better or worse than the rest?",
		Informs:   []string{ForwardInformsBuy},
		Condition: "Regime's stage at the last close",
		Unit:      ForwardUnitContractWeek, UnitText: "one contract in one week, final when its position has closed",
		ReadAt: 40, ReadGroup: "after_warning", ReadDays: 20,
		Groups: []ForwardGroup{{ID: "after_warning", Label: "after a warning"}, {ID: "other", Label: "after any other session"}},
	},
	"final_option_sales_vs_close_5_sessions": {
		Question:  "After you close an option position, is the contract worth more or less five sessions later?",
		Informs:   []string{ForwardInformsHold, ForwardInformsSell},
		Condition: "the sale price against the position's average cost",
		Unit:      ForwardUnitPosition, UnitText: "one closed single-name option position",
		ReadAt: 30, ReadGroup: ForwardReadAnyGroup, ProtectionApart: true,
		Groups: []ForwardGroup{{ID: "above_cost", Label: "sold above cost"}, {ID: "below_cost", Label: "sold below cost"}},
	},
	"floor_after_partial_sale": {
		Question:  "After a partial sale, would a floor at that sale's price have protected what was left?",
		Informs:   []string{ForwardInformsSell},
		Condition: "the remainder's statement value against the price of the last partial sale",
		Unit:      ForwardUnitPartialPosition, UnitText: "one single-name option position with a partial sale",
		ReadAt: 40, ReadGroup: ForwardGroupAll, SampleLabel: "positions", ProtectionApart: true,
	},
	"option_sales_after_adverse_move": {
		Question:  "Do you sell options too early after the underlying moved against you?",
		Informs:   []string{ForwardInformsHold, ForwardInformsSell},
		Condition: "the underlying's move over the last five sessions, in expected moves",
		Unit:      ForwardUnitNameWeek, UnitText: "one name in one week with an option sale",
		ReadAt: 50, ReadGroup: "against", ProtectionApart: true,
		Groups: []ForwardGroup{{ID: "against", Label: "after a move against"}, {ID: "in_favour", Label: "after a move in favour"}},
		Policy: "protection rules: their sales are read apart", GroupNotRead: "the underlying's move in expected moves is not computed yet",
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
	// the reply; sales then count in neither sample.
	ProtectionKnown bool
	// Zone is New York; weeks and days are its dates.
	Zone *time.Location
	// FifthSessionClose is the close of the fifth US session after the
	// session of a decision made at the given instant.
	FifthSessionClose func(time.Time) (time.Time, bool)
	// PriorSessionStage is Regime's stage recorded at the close of the US
	// session before the decision's New York date.
	PriorSessionStage func(time.Time) (string, bool)
}

// ForwardUnit is one counted unit of a rule.
type ForwardUnit struct {
	Key    string
	Group  string
	Sample string
	Date   time.Time
	State  string
	Reason string
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
	switch id {
	case "stock_sales_vs_nearby_day":
		return in.countWeeks(changes, func(c Change) bool { return stockLike(c) && longSale(c) }, true, ForwardGroupAll, in.stockSaleState)
	case "entries_vs_nearby_day":
		units := in.countWeeks(changes, func(c Change) bool {
			return longEntry(c) && (stockLike(c) || isOption(c) && singleName(c))
		}, false, ForwardGroupAll, in.entryState)
		return append(units, in.unnamed(changes, longEntry, false)...)
	case "option_buys_below_premium_line":
		return in.countOptionBuys(changes, func(Change) bool { return true }, func(Change) string { return ForwardGroupNotRead })
	case "option_buys_after_regime_warning":
		units := in.countOptionBuys(changes, singleName, in.regimeGroup)
		return append(units, in.unnamed(changes, longEntry, false)...)
	case "final_option_sales_vs_close_5_sessions":
		units := in.countFinalSales(changes)
		return append(units, in.unnamed(changes, func(c Change) bool { return c.Direction == DirectionLong && c.Action == ActionExit }, true)...)
	case "floor_after_partial_sale":
		units := in.countPartialSales(changes)
		return append(units, in.unnamed(changes, func(c Change) bool { return c.Direction == DirectionLong && c.Action == ActionTrim }, true)...)
	case "option_sales_after_adverse_move":
		units := in.countWeeks(changes, func(c Change) bool {
			return isOption(c) && longSale(c) && c.Underlying != "" && !strings.EqualFold(c.Underlying, "VIX")
		}, true, ForwardGroupNotRead, in.optionSaleState)
		return append(units, in.unnamed(changes, longSale, true)...)
	}
	return nil
}

// unnamed gives one no-reading unit per option decision of the rule's kind
// whose statement named no underlying: whether it is a single name, and
// which name it is, cannot be told, so it is said rather than dropped.
func (in ForwardCountInput) unnamed(changes []Change, kind func(Change) bool, apart bool) []ForwardUnit {
	var out []ForwardUnit
	for _, c := range changes {
		if isOption(c) && strings.TrimSpace(c.Underlying) == "" && kind(c) {
			out = append(out, ForwardUnit{Key: c.ID, Group: ForwardGroupNotRead, Sample: in.sample(c, apart), Date: in.day(c.ExecutedAt), State: forwardUnitExcluded, Reason: ForwardExcludedUnderlyingUnset})
		}
	}
	return out
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

func (in ForwardCountInput) sample(c Change, apart bool) string {
	switch {
	case !apart:
		return forwardSampleTest
	case !in.ProtectionKnown:
		return forwardSampleUnknown
	case c.ProtectionContext.MatchedExecutions > 0:
		return forwardSampleProtection
	}
	return forwardSampleTest
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
		return forwardUnitPending, ""
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
		return forwardUnitPending, ""
	}
	return forwardUnitFinal, ""
}

// optionSaleState: a closing quote stored for the fifth session, as the rule
// counts only those; a statement value of a contract still held is not one.
func (in ForwardCountInput) optionSaleState(c Change) (string, string) {
	if !in.fiveSessionsOver(c) {
		return forwardUnitPending, ""
	}
	if score, ok := fiveSessionScore(c); !ok || score.CloseSource != CloseSourceClosingQuote {
		return forwardUnitExcluded, ForwardExcludedNoClosingQuote
	}
	return forwardUnitFinal, ""
}

// countWeeks counts name-weeks. With apart set, a protection rule's sales
// form their own name-weeks. A week is final once it is over and each of
// its decisions is; a decision that can give no reading is left out of the
// week, and a week left with none gives no reading.
func (in ForwardCountInput) countWeeks(changes []Change, include func(Change) bool, apart bool, group string, state func(Change) (string, string)) []ForwardUnit {
	type acc struct {
		unit             ForwardUnit
		end              time.Time
		pending, readers int
		reason           string
	}
	var order []string
	units := map[string]*acc{}
	for _, c := range changes {
		if !include(c) {
			continue
		}
		week, end := in.week(c.ExecutedAt)
		sample := in.sample(c, apart)
		key := name(c) + ":" + week + ":" + sample
		a := units[key]
		if a == nil {
			a = &acc{unit: ForwardUnit{Key: key, Group: group, Sample: sample, Date: in.day(c.ExecutedAt)}, end: end}
			units[key] = a
			order = append(order, key)
		}
		switch st, reason := state(c); st {
		case forwardUnitPending:
			a.pending++
		case forwardUnitFinal:
			a.readers++
		default:
			a.reason = reason
		}
	}
	out := make([]ForwardUnit, 0, len(order))
	for _, key := range order {
		a := units[key]
		switch {
		case a.pending > 0 || in.AsOf.Before(a.end):
			a.unit.State = forwardUnitPending
		case a.readers > 0:
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
		return forwardUnitPending, ""
	}
	if deliveryGivesNone && (p.ClosedBy == "exercise" || p.ClosedBy == "assignment") {
		return forwardUnitExcluded, ForwardExcludedDelivered
	}
	return forwardUnitFinal, ""
}

// countOptionBuys counts contract-weeks of option buys that open or add to
// a long position, one per position and week.
func (in ForwardCountInput) countOptionBuys(changes []Change, include func(Change) bool, group func(Change) string) []ForwardUnit {
	var out []ForwardUnit
	seen := map[string]bool{}
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
		seen[key] = true
		st, reason := positionState(c, true)
		out = append(out, ForwardUnit{Key: key, Group: group(c), Sample: forwardSampleTest, Date: in.day(c.ExecutedAt), State: st, Reason: reason})
	}
	return out
}

// regimeGroup labels a buy by Regime's stage of the session before it; a
// session without a record is not read, never given a neighbour's stage.
func (in ForwardCountInput) regimeGroup(c Change) string {
	if in.PriorSessionStage == nil {
		return ForwardGroupNotRead
	}
	stage, ok := in.PriorSessionStage(c.ExecutedAt)
	if !ok {
		return ForwardGroupNotRead
	}
	return ForwardRegimeGroup(stage)
}

// ForwardRegimeGroup is the group of option_buys_after_regime_warning a buy
// after a session with this Regime stage joins: warning is early warning,
// confirmed stress or panic; the rest is quiet, stabilization or
// opportunity; any other stage is not read.
func ForwardRegimeGroup(stage string) string {
	switch stage {
	case "early_warning", "confirmed_stress", "panic":
		return "after_warning"
	case "quiet", "stabilization", "opportunity":
		return "other"
	}
	return ForwardGroupNotRead
}

// countFinalSales counts closed single-name long option positions by the
// sale that closed them. An expiry is not a sale and is not a change.
func (in ForwardCountInput) countFinalSales(changes []Change) []ForwardUnit {
	var out []ForwardUnit
	for _, c := range changes {
		if !isOption(c) || c.Direction != DirectionLong || c.Action != ActionExit || !singleName(c) {
			continue
		}
		unit := ForwardUnit{Key: c.ID, Group: ForwardGroupNotRead, Sample: in.sample(c, true), Date: in.day(c.ExecutedAt)}
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
		if unit.State != forwardUnitExcluded && unit.Group == ForwardGroupNotRead && c.Position.ResultPct != nil && *c.Position.ResultPct == 0 {
			unit.State, unit.Reason = forwardUnitExcluded, ForwardExcludedSoldAtCost
		}
		out = append(out, unit)
	}
	return out
}

// countPartialSales counts single-name long option positions with a partial
// sale in the sample. A position on which a protection rule sent any sale,
// in the window, is read apart.
func (in ForwardCountInput) countPartialSales(sampled []Change) []ForwardUnit {
	apart := map[string]bool{}
	for _, c := range in.Changes {
		if isOption(c) && longSale(c) && c.Position != nil && c.Position.PositionID != "" && c.ProtectionContext.MatchedExecutions > 0 {
			apart[c.Position.PositionID] = true
		}
	}
	var out []ForwardUnit
	seen := map[string]bool{}
	for _, c := range sampled {
		if !isOption(c) || c.Direction != DirectionLong || c.Action != ActionTrim || !singleName(c) {
			continue
		}
		unit := ForwardUnit{Key: c.ID, Group: ForwardGroupAll, Sample: forwardSampleTest, Date: in.day(c.ExecutedAt)}
		if c.Position != nil && c.Position.PositionID != "" {
			if seen[c.Position.PositionID] {
				continue
			}
			seen[c.Position.PositionID] = true
			unit.Key = c.Position.PositionID
		}
		switch {
		case !in.ProtectionKnown:
			unit.Sample = forwardSampleUnknown
		case apart[unit.Key]:
			unit.Sample = forwardSampleProtection
		}
		unit.State, unit.Reason = positionState(c, false)
		out = append(out, unit)
	}
	return out
}

// ForwardCount is a tally of final units and units not final yet.
type ForwardCount struct {
	Final   int `json:"final"`
	Pending int `json:"pending"`
}

// ForwardGroupCount is one group's tally in the test sample.
type ForwardGroupCount struct {
	ID      string `json:"id"`
	Final   int    `json:"final"`
	Pending int    `json:"pending"`
	// Days is the number of separate days the group's final units fall on.
	Days int `json:"days"`
}

// ForwardMonth is the final units of one month, by unit date.
type ForwardMonth struct {
	Month string `json:"month"`
	Final int    `json:"final"`
}

// ForwardExpected is when the read point is expected, at a measured pace.
type ForwardExpected struct {
	Date  time.Time `json:"date"`
	Basis string    `json:"basis"`
	// BeyondWindow is set when the date lies more than the review window
	// after registration: by then the first units have left the window, so
	// the rule cannot fill until counted units are stored.
	BeyondWindow bool `json:"beyond_window"`
}

// ForwardSample is the tally of one rule.
type ForwardSample struct {
	State string `json:"state"`
	// Test is the sample the rule is read on; Groups splits it.
	Test   ForwardCount        `json:"test"`
	Groups []ForwardGroupCount `json:"groups"`
	// Protection is the sample of sales a protection rule sent, read apart.
	Protection *ForwardCount `json:"protection,omitempty"`
	// SenderUnknown counts sale units whose sender could not be told this
	// time; they are in neither sample.
	SenderUnknown int              `json:"sender_unknown"`
	Excluded      map[string]int   `json:"excluded"`
	FirstUnit     time.Time        `json:"first_unit,omitzero"`
	LastUnit      time.Time        `json:"last_unit,omitzero"`
	Months        []ForwardMonth   `json:"months"`
	ToRead        int              `json:"to_read"`
	Expected      *ForwardExpected `json:"expected,omitempty"`
	NoDate        string           `json:"no_date,omitempty"`
}

// SummarizeForward tallies a rule's units since registration and dates its
// read point. paceUnits are the same kind of units from the review window
// before registration: they set a pace while fewer than three units since
// registration are final, and never enter the sample.
func SummarizeForward(facts ForwardFacts, units, paceUnits []ForwardUnit, registeredAt, asOf time.Time, zone *time.Location) ForwardSample {
	if zone == nil {
		zone = time.UTC
	}
	out := ForwardSample{State: ForwardStateCollecting, Excluded: map[string]int{}, Months: []ForwardMonth{}}
	groups := map[string]*ForwardGroupCount{}
	ids := []string{ForwardGroupAll}
	if len(facts.Groups) > 0 {
		ids = ids[:0]
		for _, g := range facts.Groups {
			ids = append(ids, g.ID)
		}
	}
	if facts.GroupNotRead != "" || len(facts.Groups) > 0 && slices.ContainsFunc(units, func(u ForwardUnit) bool { return u.Group == ForwardGroupNotRead && u.State != forwardUnitExcluded }) {
		ids = append(ids, ForwardGroupNotRead)
	}
	for _, id := range ids {
		out.Groups = append(out.Groups, ForwardGroupCount{ID: id})
	}
	for i := range out.Groups {
		groups[out.Groups[i].ID] = &out.Groups[i]
	}
	if facts.ProtectionApart {
		out.Protection = &ForwardCount{}
	}
	days := map[string]map[string]bool{}
	months := map[string]int{}
	for _, u := range units {
		if u.State == forwardUnitExcluded {
			out.Excluded[u.Reason]++
			continue
		}
		switch u.Sample {
		case forwardSampleUnknown:
			out.SenderUnknown++
			continue
		case forwardSampleProtection:
			if out.Protection != nil {
				tally(out.Protection, u)
			}
			continue
		}
		tally(&out.Test, u)
		if g := groups[u.Group]; g != nil {
			if u.State == forwardUnitFinal {
				g.Final++
				if days[u.Group] == nil {
					days[u.Group] = map[string]bool{}
				}
				days[u.Group][u.Date.Format(time.DateOnly)] = true
			} else {
				g.Pending++
			}
		}
		if out.FirstUnit.IsZero() || u.Date.Before(out.FirstUnit) {
			out.FirstUnit = u.Date
		}
		if u.Date.After(out.LastUnit) {
			out.LastUnit = u.Date
		}
		if u.State == forwardUnitFinal {
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
	readCount := func(gs []ForwardGroupCount) (int, bool) {
		best, met := 0, false
		for _, g := range gs {
			if g.ID == ForwardGroupNotRead || facts.ReadGroup != ForwardReadAnyGroup && facts.ReadGroup != g.ID {
				continue
			}
			best = max(best, g.Final)
			met = met || g.Final >= facts.ReadAt && g.Days >= facts.ReadDays
		}
		return best, met
	}
	have, met := readCount(out.Groups)
	out.ToRead = max(facts.ReadAt-have, 0)
	if met {
		out.State = ForwardStateDue
		return out
	}
	out.Expected, out.NoDate = forwardExpected(facts, out, paceUnits, registeredAt, asOf)
	return out
}

func tally(c *ForwardCount, u ForwardUnit) {
	if u.State == forwardUnitFinal {
		c.Final++
	} else {
		c.Pending++
	}
}

// forwardExpected dates the read point from the read group's pace of final
// units: since registration once three are final, otherwise over the review
// window before registration.
func forwardExpected(facts ForwardFacts, sample ForwardSample, paceUnits []ForwardUnit, registeredAt, asOf time.Time) (*ForwardExpected, string) {
	if registeredAt.IsZero() {
		return nil, ForwardNoDateNoPace
	}
	inRead := func(u ForwardUnit) bool {
		return u.State == forwardUnitFinal && u.Sample == forwardSampleTest && u.Group != ForwardGroupNotRead &&
			(facts.ReadGroup == ForwardGroupAll || facts.ReadGroup == ForwardReadAnyGroup || facts.ReadGroup == u.Group)
	}
	perGroup := func(units []ForwardUnit) map[string]int {
		out := map[string]int{}
		for _, u := range units {
			if inRead(u) {
				out[u.Group]++
			}
		}
		return out
	}
	have := map[string]int{}
	for _, g := range sample.Groups {
		if g.ID != ForwardGroupNotRead {
			have[g.ID] = g.Final
		}
	}
	basis, counts, days := ForwardPaceSinceRegistration, maps.Clone(have), asOf.Sub(registeredAt).Hours()/24
	if sample.Test.Final < 3 || days <= 0 {
		basis, days = ForwardPacePastYear, registeredAt.Sub(asOf.AddDate(0, 0, -ForwardReviewDays)).Hours()/24
		counts = perGroup(paceUnits)
	}
	best := math.Inf(1)
	for id, n := range counts {
		if n <= 0 || days <= 0 || facts.ReadGroup != ForwardGroupAll && facts.ReadGroup != ForwardReadAnyGroup && facts.ReadGroup != id {
			continue
		}
		best = min(best, float64(max(facts.ReadAt-have[id], 0))/(float64(n)/days))
	}
	if math.IsInf(best, 1) {
		switch {
		case facts.GroupNotRead != "":
			return nil, ForwardNoDateGroupNotRead
		case basis == ForwardPaceSinceRegistration:
			return nil, ForwardNoDateNoneInGroup
		case slices.ContainsFunc(paceUnits, func(u ForwardUnit) bool { return u.Group == ForwardGroupNotRead && u.State == forwardUnitFinal }):
			return nil, ForwardNoDateNotRecorded
		}
		if facts.ProtectionApart && sample.SenderUnknown > 0 {
			return nil, ForwardNoDateSenderUnread
		}
		return nil, ForwardNoDateNoPace
	}
	date := asOf.Add(time.Duration(best * 24 * float64(time.Hour)))
	return &ForwardExpected{Date: date, Basis: basis, BeyondWindow: date.After(registeredAt.AddDate(0, 0, ForwardReviewDays))}, ""
}
