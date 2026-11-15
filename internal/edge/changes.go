package edge

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
)

// PositionChange is one exact-contract position change derived from retained
// statements with Edge's own execution grouping, position anchoring and
// classification, but without a review window. The decision scorecard builds
// its ledger from these so both reads describe the same decisions. Broker
// identifiers stay internal evidence: callers never publish them.
type PositionChange struct {
	Key                   string
	AccountID             string
	ConID                 int64
	UnderlyingConID       int64
	Symbol                string
	UnderlyingSymbol      string
	AssetClass            string
	Currency              string
	Action                string
	Direction             string
	ExecutedAt            time.Time
	DeltaQuantity         float64
	PositionBefore        float64
	PositionAfter         float64
	ExecutionVWAP         *float64
	Multiplier            *float64
	ExecutionFX           *float64
	ExecutionNotionalBase *float64
	DirectCostsBase       *float64
	// ClosePrice is the broker's closing price of the contract on the
	// execution day, when the trade row states it.
	ClosePrice   *float64
	RecordIDs    []string
	ExecutionIDs []string
	OrderIDs     []string
	TradeIDs     []string
	// PathUnbalanced is true when the contract's quantity path does not
	// reconcile with the retained position snapshots.
	PathUnbalanced bool
}

// ContractMark is one broker mark of a contract on a statement day.
type ContractMark struct {
	Day   time.Time
	Price float64
}

// ContractEvent is one option lifecycle row (exercise, assignment, expiration).
type ContractEvent struct {
	Day       time.Time
	EventType string
	Quantity  *float64
	Price     *float64
}

// ChangeEvidence is everything the scorecard needs from the retained
// statements: the classified changes, instrument metadata, statement marks,
// option lifecycle events and FX rates.
type ChangeEvidence struct {
	Changes     []PositionChange
	Instruments map[int64]flexstmt.Instrument
	Marks       map[int64][]ContractMark
	Events      map[int64][]ContractEvent
	FXRates     []flexstmt.FXRate
	// LifecycleTradeIDs are the trade ids the broker's option lifecycle rows
	// (exercise, assignment, expiration and their underlying deliveries)
	// share with trade rows: such a fill is a lifecycle delivery, not a
	// decision.
	LifecycleTradeIDs map[string]bool
	// PositionsAnchored is false when no open-position snapshot exists, so
	// every position path is unproved.
	PositionsAnchored bool
}

// PositionChanges derives every classified position change in the retained
// statements, oldest first. It is deterministic under reordered evidence and
// performs no I/O.
func PositionChanges(statements []flexstmt.Statement, baseCurrency string) (ChangeEvidence, error) {
	out := ChangeEvidence{Instruments: map[int64]flexstmt.Instrument{}, Marks: map[int64][]ContractMark{}, Events: map[int64][]ContractEvent{}, LifecycleTradeIDs: map[string]bool{}}
	ev, err := collectEvidence(statements)
	if err != nil {
		return out, err
	}
	out.FXRates = append([]flexstmt.FXRate(nil), ev.fxRates...)
	out.PositionsAnchored = ev.presentSections["open_positions"] && !ev.latestOpenPositionDate.IsZero()
	for _, instrument := range ev.instruments {
		if instrument.ConID != 0 {
			out.Instruments[instrument.ConID] = instrument
		}
	}
	for _, position := range ev.positions {
		if position.ConID == 0 || position.MarkPrice == nil || *position.MarkPrice <= 0 || position.ReportDate.IsZero() {
			continue
		}
		out.Marks[position.ConID] = append(out.Marks[position.ConID], ContractMark{Day: dayStart(position.ReportDate), Price: *position.MarkPrice})
	}
	for conid, marks := range out.Marks {
		sort.Slice(marks, func(i, j int) bool { return marks[i].Day.Before(marks[j].Day) })
		out.Marks[conid] = marks
	}
	for _, event := range ev.optionEvents {
		if event.TradeID != "" {
			out.LifecycleTradeIDs[event.TradeID] = true
		}
		if event.ConID == 0 || event.Date.IsZero() {
			continue
		}
		price := cloneFloat(event.TradePrice)
		if price == nil {
			price = cloneFloat(event.MarkPrice)
		}
		out.Events[event.ConID] = append(out.Events[event.ConID], ContractEvent{Day: dayStart(event.Date), EventType: optionEventType(event.TransactionType), Quantity: cloneFloat(event.Quantity), Price: price})
	}
	for conid, events := range out.Events {
		sort.Slice(events, func(i, j int) bool { return events[i].Day.Before(events[j].Day) })
		out.Events[conid] = events
	}

	groups := groupTrades(ev.trades, baseCurrency, ev.fxRates)
	mutations := buildMutations(groups, ev)
	byConID := map[int64][]mutation{}
	for _, item := range mutations {
		byConID[item.conid] = append(byConID[item.conid], item)
	}
	unbalanced := unbalancedContracts(groups, byConID, ev)
	positions := initialPositions(groups, byConID, ev)
	groupByKey := make(map[string]groupedTrade, len(groups))
	for _, group := range groups {
		groupByKey[group.key] = group
	}
	for _, item := range mutations {
		if item.kind != "trade" {
			positions[item.conid] += item.delta
			continue
		}
		group := groupByKey[item.key]
		before := positions[group.conid]
		parts := classifyChange(before, group.delta)
		positions[group.conid] = before + group.delta
		for _, part := range parts {
			out.Changes = append(out.Changes, positionChangeFrom(group, part, before, unbalanced[group.conid]))
			before += part.delta
		}
	}
	sort.SliceStable(out.Changes, func(i, j int) bool {
		if !out.Changes[i].ExecutedAt.Equal(out.Changes[j].ExecutedAt) {
			return out.Changes[i].ExecutedAt.Before(out.Changes[j].ExecutedAt)
		}
		return out.Changes[i].Key < out.Changes[j].Key
	})
	return out, nil
}

func positionChangeFrom(group groupedTrade, part changePart, before float64, unbalanced bool) PositionChange {
	ratio := 1.0
	if !almostEqual(group.delta, 0) {
		ratio = math.Abs(part.delta / group.delta)
	}
	change := PositionChange{
		Key: group.key, AccountID: group.account, ConID: group.conid, UnderlyingConID: group.underlyingConID,
		Symbol: group.symbol, UnderlyingSymbol: strings.TrimSpace(group.underlyingSymbol), AssetClass: strings.ToUpper(strings.TrimSpace(group.assetClass)), Currency: strings.ToUpper(strings.TrimSpace(group.currency)),
		Action: part.action, Direction: part.direction, ExecutedAt: group.executedAt,
		DeltaQuantity: part.delta, PositionBefore: before, PositionAfter: before + part.delta,
		ExecutionVWAP: cloneFloat(group.vwap), Multiplier: cloneFloat(group.multiplier), ExecutionFX: cloneFloat(group.executionFX), ClosePrice: cloneFloat(group.closePrice),
		RecordIDs: append([]string(nil), group.recordIDs...), ExecutionIDs: append([]string(nil), group.executionIDs...), OrderIDs: append([]string(nil), group.orderIDs...), TradeIDs: append([]string(nil), group.tradeIDs...),
		PathUnbalanced: unbalanced,
	}
	if group.executionNotionalBase != nil {
		value := *group.executionNotionalBase * ratio
		change.ExecutionNotionalBase = &value
	}
	if group.directCostsBase != nil {
		value := *group.directCostsBase * ratio
		change.DirectCostsBase = &value
	}
	return change
}
