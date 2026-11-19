package edge

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
)

// States, reference prices and no-reading reasons of a PositionOutcome.
const (
	OptionPositionClosed   = "closed"
	OptionPositionOpen     = "open"
	OptionPositionUnproved = "unproved"

	OptionReferenceExitAverage        = "exit_average"
	OptionReferenceExitAverageAndMark = "exit_average_and_mark"
	OptionReferenceEntryAverage       = "entry_average"

	OptionOutcomeNotRetained     = "execution_not_retained"
	OptionOutcomePriceMissing    = "price_missing"
	OptionOutcomeMarkUnavailable = "mark_unavailable"
	OptionOutcomeDelivered       = "closed_by_delivery"
)

// PositionOutcome reads one option decision against the flat-to-flat position
// it belonged to. It is built from broker executions and the latest statement
// mark only: no option price is modelled and no counterfactual is claimed.
// It states facts about the fill; the grade belongs to the position
// (PositionGrade, set once the position is closed) and no fill has one.
//
// For an open or add, ResultPct compares the price paid with the average
// price the position's contracts were closed at (and the latest statement
// mark for contracts still held). For a trim or exit it compares the price
// taken with the position's average entry price; LaterExitPct then compares
// the price taken with what the contracts closed later fetched. A positive
// LaterExitPct means the contracts kept did better than the ones sold.
type PositionOutcome struct {
	State           string     `json:"state"`
	Reason          string     `json:"reason,omitempty"`
	PositionID      string     `json:"position_id,omitempty"`
	OpenedAt        *time.Time `json:"opened_at,omitempty"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	ClosedBy        string     `json:"closed_by,omitempty"`
	PositionPNLBase *float64   `json:"position_pnl_base,omitempty"`
	PositionGrade   string     `json:"position_grade,omitempty"`
	ReferenceKind   string     `json:"reference_kind,omitempty"`
	ReferencePrice  *float64   `json:"reference_price,omitempty"`
	ResultPct       *float64   `json:"result_pct,omitempty"`
	ResultBase      *float64   `json:"result_base,omitempty"`
	LaterExitPrice  *float64   `json:"later_exit_price,omitempty"`
	LaterExitPct    *float64   `json:"later_exit_pct,omitempty"`
	LaterExitBase   *float64   `json:"later_exit_base,omitempty"`
}

// OptionDecisions rolls the position outcomes up by action. The entry rows
// and the exit rows are two readings of the same booked money and are never
// added together.
type OptionDecisions struct {
	Changes     int                  `json:"changes"`
	WithOutcome int                  `json:"with_outcome"`
	Reasons     map[string]int       `json:"reasons"`
	Actions     []OptionDecisionRoll `json:"actions"`
}

// OptionDecisionRoll is one action's reading. Medians need at least
// MinimumPatternSample decisions.
type OptionDecisionRoll struct {
	Action             string   `json:"action"`
	Changes            int      `json:"changes"`
	WithResult         int      `json:"with_result"`
	Losses             int      `json:"losses"`
	MedianResultPct    *float64 `json:"median_result_pct,omitempty"`
	TotalResultBase    *float64 `json:"total_result_base,omitempty"`
	WithLaterExit      int      `json:"with_later_exit"`
	LaterExitBetter    int      `json:"later_exit_better"`
	MedianLaterExitPct *float64 `json:"median_later_exit_pct,omitempty"`
	TotalLaterExitBase *float64 `json:"total_later_exit_base,omitempty"`
}

type optionFill struct {
	at     time.Time
	qty    float64
	price  float64
	priced bool
}

type optionPosition struct {
	id        string
	state     string
	reason    string
	direction string
	openedAt  time.Time
	closedAt  time.Time
	closedBy  string
	pnlBase   *float64
	grade     string
	entries   []optionFill
	exits     []optionFill
	remaining float64
	mark      *float64
}

func optionPositionFromRows(rows []flexstmt.Trade, cycle OptionCycle, state string, remaining float64, mark *float64) *optionPosition {
	out := &optionPosition{id: cycle.ID, state: state, direction: cycle.Direction, openedAt: cycle.OpenedAt, closedBy: cycle.ClosedBy, pnlBase: cycle.RealizedPNLBase, remaining: remaining, mark: mark}
	if state == OptionPositionClosed {
		out.closedAt, out.grade = cycle.ClosedAt, cycle.Grade
	} else {
		out.closedBy = ""
	}
	openingSide := "BUY"
	if cycle.Direction == DirectionShort {
		openingSide = "SELL"
	}
	for _, row := range rows {
		fill := optionFill{at: row.ExecutedAt, qty: math.Abs(*row.Quantity)}
		if row.Price != nil && *row.Price >= 0 {
			fill.price, fill.priced = *row.Price, true
		}
		if strings.EqualFold(row.Side, openingSide) {
			out.entries = append(out.entries, fill)
		} else {
			out.exits = append(out.exits, fill)
		}
	}
	return out
}

func optionMarkPrice(positions []flexstmt.OpenPosition, conid int64) *float64 {
	for _, position := range positions {
		if position.ConID == conid && position.MarkPrice != nil && *position.MarkPrice >= 0 {
			return cloneFloat(position.MarkPrice)
		}
	}
	return nil
}

// averagePrice is the quantity-weighted price of fills, optionally with the
// contracts still held valued at the statement mark.
func averagePrice(fills []optionFill, remaining float64, mark *float64) (float64, string) {
	value, qty := 0.0, 0.0
	for _, fill := range fills {
		if !fill.priced {
			return 0, OptionOutcomePriceMissing
		}
		value += fill.qty * fill.price
		qty += fill.qty
	}
	if remaining > 0 {
		if mark == nil {
			return 0, OptionOutcomeMarkUnavailable
		}
		value += remaining * *mark
		qty += remaining
	}
	if qty <= 0 {
		return 0, OptionOutcomePriceMissing
	}
	return value / qty, ""
}

func optionPositionOutcome(change Change, group groupedTrade, ledger map[string]*optionPosition) *PositionOutcome {
	var position *optionPosition
	for _, id := range group.recordIDs {
		if found := ledger[id]; found != nil {
			position = found
			break
		}
	}
	if position == nil {
		return &PositionOutcome{State: OptionPositionUnproved, Reason: OptionOutcomeNotRetained}
	}
	out := &PositionOutcome{State: position.state, Reason: position.reason, PositionID: position.id, ClosedBy: position.closedBy, PositionPNLBase: cloneFloat(position.pnlBase)}
	if position.state == OptionPositionUnproved {
		return out
	}
	out.OpenedAt = &position.openedAt
	if position.state == OptionPositionClosed {
		out.ClosedAt, out.PositionGrade = &position.closedAt, position.grade
	} else {
		// Realized so far is not the position's result while contracts are held.
		out.PositionPNLBase = nil
	}
	// An exercise or assignment books the option at zero and moves the value
	// into the stock delivered or taken, so a price comparison would mislead.
	if position.closedBy == "exercise" || position.closedBy == "assignment" {
		out.Reason = OptionOutcomeDelivered
		return out
	}
	entry := change.Action == ActionOpen || change.Action == ActionAdd
	// A close booked at zero is a real closing price, never an entry. (An
	// expiry is no change at all: Analyze leaves it to the position.)
	if change.ExecutionVWAP == nil || *change.ExecutionVWAP < 0 || entry && *change.ExecutionVWAP == 0 {
		out.Reason = OptionOutcomePriceMissing
		return out
	}
	sign := 1.0
	if position.direction == DirectionShort {
		sign = -1
	}
	price := *change.ExecutionVWAP
	// money values the contracts of this decision over a price difference, at
	// the decision's own exchange rate and before costs.
	money := func(difference float64) *float64 {
		if group.multiplier == nil || group.executionFX == nil {
			return nil
		}
		value := sign * math.Abs(change.DeltaQuantity) * *group.multiplier * difference * *group.executionFX
		return &value
	}
	if entry {
		reference, reason := averagePrice(position.exits, position.remaining, position.mark)
		if reason != "" {
			out.Reason = reason
			return out
		}
		out.ReferenceKind = OptionReferenceExitAverage
		if position.remaining > 0 {
			out.ReferenceKind = OptionReferenceExitAverageAndMark
		}
		pct := sign * (reference/price - 1) * 100
		out.ReferencePrice, out.ResultPct, out.ResultBase = &reference, &pct, money(reference-price)
		return out
	}
	reference, reason := averagePrice(position.entries, 0, nil)
	if reason != "" || reference <= 0 {
		out.Reason = OptionOutcomePriceMissing
		return out
	}
	pct := sign * (price/reference - 1) * 100
	out.ReferenceKind, out.ReferencePrice, out.ResultPct, out.ResultBase = OptionReferenceEntryAverage, &reference, &pct, money(price-reference)
	var later []optionFill
	for _, fill := range position.exits {
		if fill.at.After(change.ExecutedAt) {
			later = append(later, fill)
		}
	}
	if price == 0 || len(later) == 0 && position.remaining <= 0 {
		return out
	}
	laterPrice, reason := averagePrice(later, position.remaining, position.mark)
	if reason != "" {
		return out
	}
	laterPct := sign * (laterPrice/price - 1) * 100
	out.LaterExitPrice, out.LaterExitPct, out.LaterExitBase = &laterPrice, &laterPct, money(laterPrice-price)
	return out
}

func buildOptionDecisions(changes []Change) OptionDecisions {
	out := OptionDecisions{Reasons: map[string]int{}, Actions: []OptionDecisionRoll{}}
	type bucket struct {
		roll           OptionDecisionRoll
		results, later []float64
		resultBase     float64
		resultKnown    int
		laterBase      float64
		laterKnown     int
	}
	buckets := map[string]*bucket{}
	for _, change := range changes {
		outcome := change.Position
		if outcome == nil {
			continue
		}
		out.Changes++
		b := buckets[change.Action]
		if b == nil {
			b = &bucket{roll: OptionDecisionRoll{Action: change.Action}}
			buckets[change.Action] = b
		}
		b.roll.Changes++
		if outcome.ResultPct == nil {
			reason := outcome.Reason
			if reason == "" {
				reason = OptionOutcomePriceMissing
			}
			out.Reasons[reason]++
			continue
		}
		out.WithOutcome++
		b.roll.WithResult++
		b.results = append(b.results, *outcome.ResultPct)
		if *outcome.ResultPct < 0 {
			b.roll.Losses++
		}
		if outcome.ResultBase != nil {
			b.resultBase += *outcome.ResultBase
			b.resultKnown++
		}
		if outcome.LaterExitPct != nil {
			b.roll.WithLaterExit++
			b.later = append(b.later, *outcome.LaterExitPct)
			if *outcome.LaterExitPct > 0 {
				b.roll.LaterExitBetter++
			}
			if outcome.LaterExitBase != nil {
				b.laterBase += *outcome.LaterExitBase
				b.laterKnown++
			}
		}
	}
	for _, action := range []string{ActionOpen, ActionAdd, ActionTrim, ActionExit} {
		b := buckets[action]
		if b == nil {
			continue
		}
		if len(b.results) >= MinimumPatternSample {
			b.roll.MedianResultPct = medianOf(b.results)
		}
		if b.resultKnown > 0 && b.resultKnown == b.roll.WithResult {
			b.roll.TotalResultBase = &b.resultBase
		}
		if len(b.later) >= MinimumPatternSample {
			b.roll.MedianLaterExitPct = medianOf(b.later)
		}
		if b.laterKnown > 0 && b.laterKnown == b.roll.WithLaterExit {
			b.roll.TotalLaterExitBase = &b.laterBase
		}
		out.Actions = append(out.Actions, b.roll)
	}
	return out
}

func medianOf(values []float64) *float64 {
	value := median(slices.Clone(values))
	return &value
}
