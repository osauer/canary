package edge

import (
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
)

// OptionCycles describes proven exact-contract positions opened and fully
// closed inside the review window. It does not infer multi-leg strategies.
// MadeMoney, LostMoney, Flat and Ungraded count Cycles by Grade.
type OptionCycles struct {
	CompletedCount    int            `json:"completed_count"`
	CompletePNLCount  int            `json:"complete_pnl_count"`
	OpenContractCount int            `json:"open_contract_count"`
	ExcludedContracts int            `json:"excluded_contracts"`
	MadeMoney         int            `json:"made_money"`
	LostMoney         int            `json:"lost_money"`
	Flat              int            `json:"flat"`
	Ungraded          int            `json:"ungraded"`
	Reasons           map[string]int `json:"reasons"`
	KnownPNLBase      *float64       `json:"known_pnl_base,omitempty"`
	Cycles            []OptionCycle  `json:"cycles"`
}

// OptionCycle is a broker-reconciled flat-to-flat exact-contract history.
// ClosedBy says how the flattening execution closed the position: trade, or
// the broker lifecycle event (expiry, exercise, assignment) whose BookTrade
// execution carries the broker's realized P/L. After an exercise or
// assignment that P/L is the option leg's alone; the stock delivered or
// taken is not part of the cycle.
type OptionCycle struct {
	ID                    string    `json:"id"`
	Symbol                string    `json:"symbol"`
	Direction             string    `json:"direction"`
	OpenedAt              time.Time `json:"opened_at"`
	ClosedAt              time.Time `json:"closed_at"`
	ClosedBy              string    `json:"closed_by"`
	ExecutionCount        int       `json:"execution_count"`
	RealizedPNLBase       *float64  `json:"realized_pnl_base,omitempty"`
	PNLStatus             string    `json:"pnl_status"`
	MissingEvidence       []string  `json:"missing_evidence"`
	LinkedProtectionCount int       `json:"linked_protection_count"`
	// Grade is the position's one grade; GradeReason says why it is none.
	Grade       string `json:"grade"`
	GradeReason string `json:"grade_reason,omitempty"`
}

// Grades of a completed option position, and why one has none.
const (
	OptionGradeMadeMoney = "made_money"
	OptionGradeLostMoney = "lost_money"
	OptionGradeFlat      = "flat"
	OptionGradeNone      = "none"

	OptionGradeResultIncomplete = "result_incomplete"
)

// optionCycleGrade grades a completed position by the sign of the broker's
// realized result for all of its executions, costs included, and by nothing
// else. Profit grades the position (owner, 2026-11-19 06:45 CET): a price
// read after a trade never enters, so a grade cannot change when a later
// close is fetched. A position closed by exercise or assignment has no
// grade, because the broker books the option leg alone and the value moved
// into the stock delivered or taken.
func optionCycleGrade(cycle OptionCycle) (grade, reason string) {
	switch {
	case cycle.ClosedBy == "exercise" || cycle.ClosedBy == "assignment":
		return OptionGradeNone, OptionOutcomeDelivered
	case cycle.RealizedPNLBase == nil || cycle.PNLStatus != OptionPNLComplete:
		return OptionGradeNone, OptionGradeResultIncomplete
	case almostEqual(*cycle.RealizedPNLBase, 0):
		return OptionGradeFlat, ""
	case *cycle.RealizedPNLBase > 0:
		return OptionGradeMadeMoney, ""
	default:
		return OptionGradeLostMoney, ""
	}
}

func buildOptionCycles(ev evidence, from, to time.Time, base string, protection map[string]ProtectionRecord, initial map[int64]float64, unbalanced map[int64]bool) OptionCycles {
	out, _ := buildOptionPositions(ev, from, to, base, protection, initial, unbalanced)
	return out
}

// buildOptionPositions walks every option contract once. It returns the
// in-window cycles and a ledger from each retained execution to the
// flat-to-flat position it belonged to, so a decision can be read against
// what its position went on to do.
func buildOptionPositions(ev evidence, from, to time.Time, base string, protection map[string]ProtectionRecord, initial map[int64]float64, unbalanced map[int64]bool) (OptionCycles, map[string]*optionPosition) {
	out := OptionCycles{Reasons: map[string]int{}, Cycles: []OptionCycle{}}
	ledger := map[string]*optionPosition{}
	link := func(rows []flexstmt.Trade, position *optionPosition) {
		for _, row := range rows {
			ledger[row.RecordID] = position
		}
	}
	byContract := map[int64][]flexstmt.Trade{}
	executed := map[string]bool{}
	for _, trade := range ev.trades {
		if !strings.EqualFold(trade.AssetClass, "OPT") || trade.ConID == 0 {
			continue
		}
		if trade.LevelOfDetail != "" && trade.LevelOfDetail != "EXECUTION" && trade.LevelOfDetail != "EXECUTIONS" {
			continue
		}
		if trade.TradeID != "" {
			executed[lifecycleTradeKey(trade.ConID, trade.TradeID)] = true
		}
		if tradeStatementDay(trade).After(dayStart(to)) {
			continue
		}
		byContract[trade.ConID] = append(byContract[trade.ConID], trade)
	}
	// An expiry, exercise or assignment whose BookTrade execution was
	// retained is closed by that execution, which carries the broker's
	// realized P/L. Any other lifecycle row keeps the contract unproved.
	closedBy := map[string]string{}
	contaminated := map[int64]bool{}
	for _, event := range ev.optionEvents {
		key := lifecycleTradeKey(event.ConID, event.TradeID)
		if kind := optionCycleCloser(event.TransactionType); event.TradeID != "" && executed[key] && kind != "" {
			closedBy[key] = kind
			continue
		}
		contaminated[event.ConID] = true
	}
	for _, event := range ev.transfers {
		contaminated[event.ConID] = true
	}
	for _, event := range ev.corporateActions {
		contaminated[event.ConID] = true
	}
	ids := make([]int64, 0, len(byContract))
	for id := range byContract {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	total, known := 0.0, 0
	for _, id := range ids {
		rows := byContract[id]
		sort.Slice(rows, func(i, j int) bool {
			if !rows[i].ExecutedAt.Equal(rows[j].ExecutedAt) {
				return rows[i].ExecutedAt.Before(rows[j].ExecutedAt)
			}
			return rows[i].RecordID < rows[j].RecordID
		})
		reason := ""
		switch {
		case !ev.presentSections["open_positions"] || !ev.presentSections["trades"] || ev.latestOpenPositionDate.IsZero() || optionCycleAnchorUnknown(ev.positions, id):
			reason = "quantity_anchor_unavailable"
		case unbalanced[id]:
			reason = ReasonPositionPathUnbalanced
		case contaminated[id]:
			reason = "lifecycle_or_transfer"
		}
		if reason == "" {
			reason = optionCyclePathReason(rows, initial[id])
		}
		if reason != "" {
			out.ExcludedContracts++
			out.Reasons[reason]++
			link(rows, &optionPosition{state: OptionPositionUnproved, reason: reason})
			continue
		}
		quantity := initial[id]
		var active []flexstmt.Trade
		startedFlat := false
		for _, row := range rows {
			delta := math.Abs(*row.Quantity)
			if strings.EqualFold(row.Side, "SELL") {
				delta = -delta
			}
			before := quantity
			quantity += delta
			if almostEqual(before, 0) {
				active = nil
				startedFlat = true
			}
			active = append(active, row)
			if !almostEqual(quantity, 0) {
				continue
			}
			// A nonzero initial position can close without a retained opening.
			if !startedFlat || len(active) == 0 || !strings.EqualFold(active[0].OpenClose, "O") {
				out.Reasons["opening_unproved"]++
				link(active, &optionPosition{state: OptionPositionUnproved, reason: "opening_unproved"})
				active = nil
				continue
			}
			cycle := optionCycleFromRows(active, base, ev.fxRates, protection)
			cycle.ClosedBy = "trade"
			if last := active[len(active)-1]; last.TradeID != "" && closedBy[lifecycleTradeKey(last.ConID, last.TradeID)] != "" {
				cycle.ClosedBy = closedBy[lifecycleTradeKey(last.ConID, last.TradeID)]
			}
			cycle.Grade, cycle.GradeReason = optionCycleGrade(cycle)
			// A position opened before the window is not an in-window cycle,
			// but the decisions made in it inside the window still have its
			// outcome.
			link(active, optionPositionFromRows(active, cycle, OptionPositionClosed, 0, nil))
			if tradeStatementDay(active[0]).Before(dayStart(from)) {
				out.Reasons["opened_before_window"]++
				active = nil
				continue
			}
			out.CompletedCount++
			if cycle.PNLStatus == OptionPNLComplete {
				out.CompletePNLCount++
			}
			if cycle.RealizedPNLBase != nil {
				total += *cycle.RealizedPNLBase
				known++
			}
			switch cycle.Grade {
			case OptionGradeMadeMoney:
				out.MadeMoney++
			case OptionGradeLostMoney:
				out.LostMoney++
			case OptionGradeFlat:
				out.Flat++
			default:
				out.Ungraded++
			}
			out.Cycles = append(out.Cycles, cycle)
			active = nil
		}
		if !almostEqual(quantity, 0) {
			out.OpenContractCount++
			if len(active) > 0 {
				if !startedFlat || !strings.EqualFold(active[0].OpenClose, "O") {
					link(active, &optionPosition{state: OptionPositionUnproved, reason: "opening_unproved"})
				} else {
					cycle := optionCycleFromRows(active, base, ev.fxRates, protection)
					link(active, optionPositionFromRows(active, cycle, OptionPositionOpen, math.Abs(quantity), optionMarkPrice(ev.currentPositions, id)))
				}
			}
		}
	}
	if known > 0 {
		out.KnownPNLBase = &total
	}
	sort.Slice(out.Cycles, func(i, j int) bool {
		a, b := 0.0, 0.0
		if out.Cycles[i].RealizedPNLBase != nil {
			a = math.Abs(*out.Cycles[i].RealizedPNLBase)
		}
		if out.Cycles[j].RealizedPNLBase != nil {
			b = math.Abs(*out.Cycles[j].RealizedPNLBase)
		}
		if a != b {
			return a > b
		}
		return out.Cycles[i].ID < out.Cycles[j].ID
	})
	return out, ledger
}

// optionCycleCloser names the lifecycle events that close an exact-contract
// position with a broker-reported realized P/L; anything else is "".
func optionCycleCloser(transactionType string) string {
	switch optionEventType(transactionType) {
	case "expiration":
		return "expiry"
	case "exercise", "assignment":
		return optionEventType(transactionType)
	default:
		return ""
	}
}

func optionCyclePathReason(rows []flexstmt.Trade, quantity float64) string {
	for i, row := range rows {
		if row.Quantity == nil || row.ExecutedAt.IsZero() || almostEqual(*row.Quantity, 0) || !strings.EqualFold(row.Side, "BUY") && !strings.EqualFold(row.Side, "SELL") {
			return "execution_path_incomplete"
		}
		if i > 0 && row.ExecutedAt.Equal(rows[i-1].ExecutedAt) && !strings.EqualFold(row.Side, rows[i-1].Side) {
			return "execution_order_ambiguous"
		}
		delta := math.Abs(*row.Quantity)
		if strings.EqualFold(row.Side, "SELL") {
			delta = -delta
		}
		after := quantity + delta
		opening := almostEqual(quantity, 0) || quantity*delta > 0
		if opening && !strings.EqualFold(row.OpenClose, "O") || !opening && !strings.EqualFold(row.OpenClose, "C") {
			return "open_close_conflict"
		}
		if !almostEqual(quantity, 0) && quantity*after < 0 && !almostEqual(after, 0) {
			return "cross_zero_execution"
		}
		quantity = after
	}
	return ""
}

func optionCycleFromRows(rows []flexstmt.Trade, base string, rates []flexstmt.FXRate, protection map[string]ProtectionRecord) OptionCycle {
	first, last := rows[0], rows[len(rows)-1]
	out := OptionCycle{ID: opaqueID("option-cycle", first.RecordID, last.RecordID), Symbol: first.Symbol, Direction: DirectionLong, OpenedAt: first.ExecutedAt, ClosedAt: last.ExecutedAt, ExecutionCount: len(rows)}
	if strings.EqualFold(first.Side, "SELL") {
		out.Direction = DirectionShort
	}
	total, known, missing := 0.0, 0, 0
	reasons := map[string]bool{}
	for _, row := range rows {
		if _, ok := protection[row.RecordID]; ok {
			out.LinkedProtectionCount++
		}
		fx := baseConversionFX(row.Currency, base, row.ExecutedAt, row.FXRateToBase, rates)
		if row.RealizedPNL == nil {
			missing++
			reasons[OptionMissingRealizedPNL] = true
		} else if fx == nil {
			missing++
			reasons[OptionMissingFX] = true
		} else {
			total += *row.RealizedPNL * *fx
			known++
		}
	}
	out.RealizedPNLBase, out.PNLStatus = optionPNLState(total, known, missing)
	out.MissingEvidence = sortedKeys(reasons)
	return out
}

func optionCycleAnchorUnknown(positions []flexstmt.OpenPosition, id int64) bool {
	for _, p := range positions {
		if p.ConID == id && (p.Quantity == nil || p.ReportDate.IsZero()) {
			return true
		}
	}
	return false
}
