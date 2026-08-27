package edge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

type versioned[T any] struct {
	generated time.Time
	tie       string
	value     T
}

type evidence struct {
	statements             []flexstmt.Statement
	trades                 []flexstmt.Trade
	instruments            []flexstmt.Instrument
	positions              []flexstmt.OpenPosition
	currentPositions       []flexstmt.OpenPosition
	optionEvents           []flexstmt.OptionEvent
	corporateActions       []flexstmt.CorporateAction
	transfers              []flexstmt.Transfer
	fxRates                []flexstmt.FXRate
	equity                 []equityEvidence
	cash                   []cashEvidence
	latestOpenPositionDate time.Time
	presentSections        map[string]bool
}

type positionSnapshot struct {
	through   time.Time
	generated time.Time
	tie       string
	positions []flexstmt.OpenPosition
}

type equityEvidence struct {
	account string
	row     flexstmt.EquityRow
}

type cashEvidence struct {
	account string
	line    flexstmt.CashLine
}

type groupedTrade struct {
	key             string
	account         string
	conid           int64
	symbol          string
	assetClass      string
	currency        string
	side            string
	executedAt      time.Time
	delta           float64
	vwap            *float64
	multiplier      *float64
	directCostsBase *float64
	recordIDs       []string
	queryComplete   bool
	costFXMissing   bool
}

type mutation struct {
	conid int64
	at    time.Time
	delta float64
	kind  string
	key   string
}

type scoringIndex struct {
	mutationsByConID map[int64][]mutation
	barsByConID      map[int64][]DailyBar
	rawBarsByConID   map[int64]bool
	contextBars      map[string][]DailyBar
}

// Analyze deterministically computes one 90- or 365-day Edge result.
func Analyze(input Input) (Result, error) {
	if input.WindowDays != 90 && input.WindowDays != 365 {
		return Result{}, fmt.Errorf("edge window must be 90 or 365 days")
	}
	ev, err := collectEvidence(input.Statements)
	if err != nil {
		return Result{}, err
	}
	asOf := input.AsOf.UTC()
	if asOf.IsZero() {
		asOf = evidenceAsOf(ev)
	}
	if asOf.IsZero() {
		asOf = time.Unix(0, 0).UTC()
	}
	input.AsOf = asOf
	result := Result{
		SchemaVersion: SchemaVersion,
		AsOf:          asOf,
		WindowDays:    input.WindowDays,
		Coverage:      Coverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}},
		Method:        defaultMethod(),
		NotExecution:  true,
	}
	applySectionCoverage(&result.Coverage, ev)
	result.Account = accountResult(ev, asOf, input.WindowDays, strings.ToUpper(strings.TrimSpace(input.BaseCurrency)), &result.Coverage)

	groups := groupTrades(ev.trades, input.BaseCurrency, ev.fxRates)
	mutations := buildMutations(groups, ev)
	index := buildScoringIndex(input, mutations)
	unbalanced := unbalancedContracts(groups, index.mutationsByConID, ev)
	windowStart := asOf.AddDate(0, 0, -input.WindowDays)
	positions := initialPositions(groups, index.mutationsByConID, ev)
	groupByKey := make(map[string]groupedTrade, len(groups))
	for _, group := range groups {
		groupByKey[group.key] = group
	}
	for _, mutation := range mutations {
		if mutation.kind != "trade" {
			positions[mutation.conid] += mutation.delta
			continue
		}
		group, ok := groupByKey[mutation.key]
		if !ok {
			return Result{}, fmt.Errorf("edge trade mutation lost its execution group")
		}
		before := positions[group.conid]
		parts := classifyChange(before, group.delta)
		positions[group.conid] = before + group.delta
		for _, part := range parts {
			if group.executedAt.Before(windowStart) || group.executedAt.After(asOf) {
				continue
			}
			pathReason := ""
			if !ev.presentSections["open_positions"] {
				pathReason = ReasonQueryFieldMissing
			} else if unbalanced[group.conid] {
				pathReason = ReasonPositionPathUnbalanced
			}
			change := buildChange(group, part, before, input, index, ev.fxRates, pathReason)
			before += part.delta
			result.Changes = append(result.Changes, change)
		}
	}
	sort.Slice(result.Changes, func(i, j int) bool {
		if !result.Changes[i].ExecutedAt.Equal(result.Changes[j].ExecutedAt) {
			return result.Changes[i].ExecutedAt.Before(result.Changes[j].ExecutedAt)
		}
		return result.Changes[i].ID < result.Changes[j].ID
	})
	result.Rollups = buildRollups(result.Changes)
	startingEquity := 0.0
	if result.Account != nil {
		startingEquity = result.Account.StartingEquityBase
	}
	result.Findings = buildFindings(result.Changes, startingEquity)
	result.Options = buildOptionReview(ev, windowStart, asOf, input.BaseCurrency, ev.fxRates)
	populateCoverage(&result.Coverage, result.Changes)
	result.Fingerprint, err = fingerprint(result)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func collectEvidence(statements []flexstmt.Statement) (evidence, error) {
	ev := evidence{statements: append([]flexstmt.Statement(nil), statements...), presentSections: make(map[string]bool)}
	tradeW := map[string]versioned[flexstmt.Trade]{}
	instrumentW := map[string]versioned[flexstmt.Instrument]{}
	positionW := map[string]versioned[flexstmt.OpenPosition]{}
	positionSnapshots := map[string]positionSnapshot{}
	optionW := map[string]versioned[flexstmt.OptionEvent]{}
	corpW := map[string]versioned[flexstmt.CorporateAction]{}
	fxW := map[string]versioned[flexstmt.FXRate]{}
	equityW := map[string]versioned[equityEvidence]{}
	cashW := map[string]versioned[cashEvidence]{}
	transferW := map[string]versioned[flexstmt.Transfer]{}
	for _, st := range statements {
		for _, coverage := range st.Coverage {
			if coverage.Present {
				ev.presentSections[coverage.Key] = true
			}
		}
		generated := st.WhenGenerated.UTC()
		for _, item := range st.Trades {
			chooseWinner(tradeW, item.RecordID, generated, item)
		}
		for _, item := range st.Instruments {
			if item.ConID != 0 {
				chooseWinner(instrumentW, strconv.FormatInt(item.ConID, 10), generated, item)
			}
		}
		for _, item := range st.Positions {
			chooseWinner(positionW, item.RecordID, generated, item)
		}
		if sectionPresent(st.Coverage, "open_positions") {
			latestRowDate := time.Time{}
			for _, item := range st.Positions {
				if item.ReportDate.After(latestRowDate) {
					latestRowDate = item.ReportDate
				}
			}
			positions := make([]flexstmt.OpenPosition, 0, len(st.Positions))
			for _, item := range st.Positions {
				if latestRowDate.IsZero() || item.ReportDate.Equal(latestRowDate) {
					positions = append(positions, item)
				}
			}
			through := st.ToDate
			if through.IsZero() || latestRowDate.After(through) {
				through = latestRowDate
			}
			raw, _ := json.Marshal(positions)
			candidate := positionSnapshot{through: through, generated: generated, tie: string(raw), positions: positions}
			account := st.AccountID
			current, ok := positionSnapshots[account]
			if !ok || through.After(current.through) ||
				(through.Equal(current.through) && (generated.After(current.generated) || generated.Equal(current.generated) && candidate.tie > current.tie)) {
				positionSnapshots[account] = candidate
			}
		}
		for _, item := range st.OptionEvents {
			chooseWinner(optionW, item.RecordID, generated, item)
		}
		for _, item := range st.CorporateActions {
			chooseWinner(corpW, item.RecordID, generated, item)
		}
		for _, item := range st.FXRates {
			chooseWinner(fxW, item.RecordID, generated, item)
		}
		for _, item := range st.Equity {
			key := st.AccountID + "\x00" + dayKey(item.ReportDate)
			chooseWinner(equityW, key, generated, equityEvidence{account: st.AccountID, row: item})
		}
		for _, item := range st.Cash {
			key := st.AccountID + "\x00" + item.ID
			chooseWinner(cashW, key, generated, cashEvidence{account: st.AccountID, line: item})
		}
		for _, item := range st.Transfers {
			key := firstNonEmpty(item.AccountID, st.AccountID) + "\x00" + item.ID
			chooseWinner(transferW, key, generated, item)
		}
	}
	ev.trades = winnerValues(tradeW)
	ev.instruments = winnerValues(instrumentW)
	ev.positions = winnerValues(positionW)
	ev.optionEvents = winnerValues(optionW)
	ev.corporateActions = winnerValues(corpW)
	ev.fxRates = winnerValues(fxW)
	ev.equity = winnerValues(equityW)
	ev.cash = winnerValues(cashW)
	ev.transfers = winnerValues(transferW)
	accounts := make([]string, 0, len(positionSnapshots))
	for account := range positionSnapshots {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		snapshot := positionSnapshots[account]
		ev.currentPositions = append(ev.currentPositions, snapshot.positions...)
		if snapshot.through.After(ev.latestOpenPositionDate) {
			ev.latestOpenPositionDate = snapshot.through
		}
	}
	sort.Slice(ev.currentPositions, func(i, j int) bool { return ev.currentPositions[i].RecordID < ev.currentPositions[j].RecordID })
	sort.Slice(ev.trades, func(i, j int) bool {
		if !ev.trades[i].ExecutedAt.Equal(ev.trades[j].ExecutedAt) {
			return ev.trades[i].ExecutedAt.Before(ev.trades[j].ExecutedAt)
		}
		return ev.trades[i].RecordID < ev.trades[j].RecordID
	})
	return ev, nil
}

func chooseWinner[T any](dest map[string]versioned[T], key string, generated time.Time, value T) {
	raw, _ := json.Marshal(value)
	candidate := versioned[T]{generated: generated, tie: string(raw), value: value}
	current, ok := dest[key]
	if !ok || generated.After(current.generated) || (generated.Equal(current.generated) && candidate.tie > current.tie) {
		dest[key] = candidate
	}
}

func winnerValues[T any](source map[string]versioned[T]) []T {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(keys))
	for _, key := range keys {
		out = append(out, source[key].value)
	}
	return out
}

func evidenceAsOf(ev evidence) time.Time {
	var out time.Time
	for _, item := range ev.equity {
		if item.row.ReportDate.After(out) {
			out = item.row.ReportDate
		}
	}
	for _, st := range ev.statements {
		if st.ToDate.After(out) {
			out = st.ToDate
		}
	}
	return out.UTC()
}

func applySectionCoverage(out *Coverage, ev evidence) {
	for _, section := range flexstmt.CanonicalQueryManifest() {
		if ev.presentSections[section.Key] {
			out.PresentSections = append(out.PresentSections, section.Key)
		} else {
			out.MissingSections = append(out.MissingSections, section.Key)
		}
	}
}

func accountResult(ev evidence, asOf time.Time, days int, base string, coverage *Coverage) *AccountResult {
	if len(ev.equity) < 2 {
		coverage.ReasonCounts[ReasonQueryFieldMissing]++
		return nil
	}
	sort.Slice(ev.equity, func(i, j int) bool {
		if !ev.equity[i].row.ReportDate.Equal(ev.equity[j].row.ReportDate) {
			return ev.equity[i].row.ReportDate.Before(ev.equity[j].row.ReportDate)
		}
		return ev.equity[i].account < ev.equity[j].account
	})
	// Consolidate same-day account rows without exposing account identities.
	byDay := map[string]float64{}
	for _, item := range ev.equity {
		if !item.row.ReportDate.After(asOf) {
			byDay[dayKey(item.row.ReportDate)] += item.row.TotalBase
		}
	}
	dayNames := make([]string, 0, len(byDay))
	for day := range byDay {
		dayNames = append(dayNames, day)
	}
	sort.Strings(dayNames)
	if len(dayNames) < 2 {
		coverage.ReasonCounts[ReasonQueryFieldMissing]++
		return nil
	}
	requested := asOf.AddDate(0, 0, -days)
	startIdx := sort.SearchStrings(dayNames, dayKey(requested))
	if startIdx >= len(dayNames)-1 {
		startIdx = len(dayNames) - 2
	}
	startDay, _ := time.Parse("2006-01-02", dayNames[startIdx])
	endDay, _ := time.Parse("2006-01-02", dayNames[len(dayNames)-1])
	flows := 0.0
	// Transfers can carry both cash and in-kind value. When the same exact
	// broker transaction also appears in Cash Transactions, the Transfer row
	// is the more complete external-flow record and wins once.
	transferTransactions := map[string]bool{}
	for _, item := range ev.transfers {
		if !afterStartThroughEnd(item.Date, startDay, endDay) {
			continue
		}
		if item.AmountBase == nil {
			coverage.ReasonCounts[ReasonMissingFX]++
			return nil
		}
		value := *item.AmountBase
		if strings.EqualFold(item.Direction, "OUT") && value > 0 {
			value = -value
		}
		if strings.EqualFold(item.Direction, "IN") && value < 0 {
			value = -value
		}
		flows += value
		if item.TransactionID != "" {
			transferTransactions[item.TransactionID] = true
		}
	}
	for _, item := range ev.cash {
		if item.line.Category != flexstmt.CategoryFlow || !afterStartThroughEnd(item.line.ValueDate, startDay, endDay) || transferTransactions[item.line.TransactionID] && item.line.TransactionID != "" {
			continue
		}
		if item.line.AmountBase == nil {
			coverage.ReasonCounts[ReasonMissingFX]++
			return nil
		}
		flows += *item.line.AmountBase
	}
	start, end := byDay[dayNames[startIdx]], byDay[dayNames[len(dayNames)-1]]
	return &AccountResult{BaseCurrency: strings.ToUpper(strings.TrimSpace(base)), RequestedFrom: requested, ActualFrom: startDay, ActualTo: endDay, StartingEquityBase: start, EndingEquityBase: end, ExternalFlowsBase: flows, ProfitLossBase: end - start - flows, Definition: "ending equity - starting equity - statement-confirmed external flows"}
}

func groupTrades(trades []flexstmt.Trade, baseCurrency string, fxRates []flexstmt.FXRate) []groupedTrade {
	groups := map[string][]flexstmt.Trade{}
	for _, trade := range trades {
		if trade.ExecutedAt.IsZero() || trade.ConID == 0 {
			continue
		}
		if trade.LevelOfDetail != "" && trade.LevelOfDetail != "EXECUTION" && trade.LevelOfDetail != "EXECUTIONS" {
			continue
		}
		key := "execution:" + trade.RecordID
		if trade.OrderID != "" {
			key = strings.Join([]string{"order", trade.AccountID, trade.OrderID, dayKey(trade.ExecutedAt), strconv.FormatInt(trade.ConID, 10), strings.ToUpper(trade.Side)}, "\x00")
		}
		groups[key] = append(groups[key], trade)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]groupedTrade, 0, len(keys))
	for _, key := range keys {
		rows := groups[key]
		sort.Slice(rows, func(i, j int) bool { return rows[i].RecordID < rows[j].RecordID })
		first := rows[0]
		g := groupedTrade{key: key, account: first.AccountID, conid: first.ConID, symbol: first.Symbol, assetClass: first.AssetClass, currency: first.Currency, side: strings.ToUpper(first.Side), executedAt: first.ExecutedAt, queryComplete: true}
		var weighted, weight, costs float64
		haveCost := true
		var multiplier *float64
		for _, row := range rows {
			g.recordIDs = append(g.recordIDs, row.RecordID)
			if row.ExecutedAt.After(g.executedAt) {
				g.executedAt = row.ExecutedAt
			}
			if row.Quantity != nil {
				q := math.Abs(*row.Quantity)
				signed := q
				if strings.EqualFold(row.Side, "SELL") {
					signed = -q
				} else if !strings.EqualFold(row.Side, "BUY") {
					signed = *row.Quantity
				}
				g.delta += signed
				if row.Price != nil {
					weighted += q * *row.Price
					weight += q
				}
			}
			if row.Quantity == nil || row.Price == nil || row.Multiplier == nil || row.Commission == nil || row.Taxes == nil || row.LevelOfDetail == "" || row.AssetClass == "" || row.Currency == "" ||
				(!strings.EqualFold(row.Side, "BUY") && !strings.EqualFold(row.Side, "SELL")) ||
				!strings.EqualFold(row.AssetClass, first.AssetClass) || !strings.EqualFold(row.Currency, first.Currency) || row.Symbol != first.Symbol {
				g.queryComplete = false
			}
			if row.Multiplier != nil {
				if multiplier == nil {
					value := *row.Multiplier
					multiplier = &value
				} else if !almostEqual(*multiplier, *row.Multiplier) {
					multiplier = nil
				}
			}
			if row.Commission == nil || row.Taxes == nil {
				haveCost = false
			} else {
				tradeFX := baseConversionFX(row.Currency, baseCurrency, row.ExecutedAt, row.FXRateToBase, fxRates)
				commissionCurrency := firstNonEmpty(row.CommissionCurrency, row.Currency)
				commissionFX := baseConversionFX(commissionCurrency, baseCurrency, row.ExecutedAt, nil, fxRates)
				if strings.EqualFold(commissionCurrency, row.Currency) {
					commissionFX = tradeFX
				}
				if tradeFX == nil || commissionFX == nil {
					haveCost = false
					g.costFXMissing = true
				} else {
					costs += math.Abs(*row.Commission) * *commissionFX
					costs += math.Abs(*row.Taxes) * *tradeFX
				}
			}
		}
		if weight > 0 {
			value := weighted / weight
			g.vwap = &value
		}
		g.multiplier = multiplier
		if haveCost {
			g.directCostsBase = &costs
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].executedAt.Equal(out[j].executedAt) {
			return out[i].executedAt.Before(out[j].executedAt)
		}
		return out[i].key < out[j].key
	})
	return out
}

func buildMutations(groups []groupedTrade, ev evidence) []mutation {
	out := make([]mutation, 0, len(groups)+len(ev.transfers)+len(ev.optionEvents)+len(ev.corporateActions))
	for _, group := range groups {
		out = append(out, mutation{conid: group.conid, at: group.executedAt, delta: group.delta, kind: "trade", key: group.key})
	}
	for _, item := range ev.transfers {
		if item.ConID == 0 || item.Quantity == nil {
			continue
		}
		delta := math.Abs(*item.Quantity)
		if strings.EqualFold(item.Direction, "OUT") {
			delta = -delta
		} else if !strings.EqualFold(item.Direction, "IN") {
			delta = *item.Quantity
		}
		out = append(out, mutation{conid: item.ConID, at: item.Date, delta: delta, kind: "transfer", key: item.ID})
	}
	for _, item := range ev.optionEvents {
		if item.ConID != 0 && item.Quantity != nil {
			out = append(out, mutation{conid: item.ConID, at: item.Date, delta: *item.Quantity, kind: "option_event", key: item.RecordID})
		}
	}
	for _, item := range ev.corporateActions {
		if item.ConID != 0 && item.Quantity != nil && *item.Quantity != 0 {
			out = append(out, mutation{conid: item.ConID, at: item.Date, delta: *item.Quantity, kind: "corporate_action", key: item.RecordID})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].key < out[j].key
	})
	return out
}

func buildScoringIndex(input Input, mutations []mutation) scoringIndex {
	index := scoringIndex{
		mutationsByConID: make(map[int64][]mutation),
		barsByConID:      make(map[int64][]DailyBar, len(input.Bars)),
		rawBarsByConID:   make(map[int64]bool, len(input.Bars)),
		contextBars:      make(map[string][]DailyBar, len(input.ContextBars)),
	}
	for _, item := range mutations {
		index.mutationsByConID[item.conid] = append(index.mutationsByConID[item.conid], item)
	}
	for conid, bars := range input.Bars {
		index.rawBarsByConID[conid] = len(bars) > 0
		index.barsByConID[conid] = normalizedBars(bars, input.AsOf)
	}
	for symbol, bars := range input.ContextBars {
		index.contextBars[strings.ToUpper(strings.TrimSpace(symbol))] = normalizedBars(bars, input.AsOf)
	}
	return index
}

func initialPositions(groups []groupedTrade, mutationsByConID map[int64][]mutation, ev evidence) map[int64]float64 {
	current := map[int64]flexstmt.OpenPosition{}
	for _, position := range ev.currentPositions {
		if position.Quantity == nil {
			continue
		}
		current[position.ConID] = position
	}
	all := map[int64]bool{}
	for _, group := range groups {
		all[group.conid] = true
	}
	for conid := range mutationsByConID {
		all[conid] = true
	}
	out := make(map[int64]float64, len(all))
	for conid := range all {
		anchor := 0.0
		anchorDate := ev.latestOpenPositionDate
		if position, ok := current[conid]; ok {
			anchor = *position.Quantity
		}
		for _, mutation := range mutationsByConID[conid] {
			if !mutation.at.After(endOfDay(anchorDate)) {
				anchor -= mutation.delta
			}
		}
		out[conid] = anchor
	}
	return out
}

func unbalancedContracts(groups []groupedTrade, mutationsByConID map[int64][]mutation, ev evidence) map[int64]bool {
	out := map[int64]bool{}
	byConID := map[int64][]flexstmt.OpenPosition{}
	known := map[int64]bool{}
	for _, anchor := range ev.positions {
		if anchor.Quantity != nil && anchor.ConID != 0 {
			byConID[anchor.ConID] = append(byConID[anchor.ConID], anchor)
			known[anchor.ConID] = true
		}
	}
	for _, group := range groups {
		known[group.conid] = true
	}
	for conid := range mutationsByConID {
		known[conid] = true
	}
	current := map[int64]float64{}
	for _, anchor := range ev.currentPositions {
		if anchor.Quantity != nil && anchor.ConID != 0 {
			current[anchor.ConID] = *anchor.Quantity
		}
	}
	if !ev.latestOpenPositionDate.IsZero() {
		for conid := range known {
			quantity := current[conid]
			alreadyPresent := false
			for _, anchor := range byConID[conid] {
				if anchor.ReportDate.Equal(ev.latestOpenPositionDate) && almostEqual(*anchor.Quantity, quantity) {
					alreadyPresent = true
					break
				}
			}
			if !alreadyPresent {
				value := quantity
				byConID[conid] = append(byConID[conid], flexstmt.OpenPosition{ConID: conid, ReportDate: ev.latestOpenPositionDate, Quantity: &value})
			}
		}
	}
	for conid, anchors := range byConID {
		sort.Slice(anchors, func(i, j int) bool { return anchors[i].ReportDate.Before(anchors[j].ReportDate) })
		if len(anchors) < 2 {
			continue
		}
		position := *anchors[0].Quantity
		for i := 1; i < len(anchors); i++ {
			for _, mutation := range mutationsByConID[conid] {
				if mutation.at.After(endOfDay(anchors[i-1].ReportDate)) && !mutation.at.After(endOfDay(anchors[i].ReportDate)) {
					position += mutation.delta
				}
			}
			if !almostEqual(position, *anchors[i].Quantity) {
				out[conid] = true
				break
			}
			position = *anchors[i].Quantity
		}
	}
	return out
}

type changePart struct {
	action, direction string
	delta             float64
}

func classifyChange(before, delta float64) []changePart {
	if almostEqual(delta, 0) {
		return nil
	}
	after := before + delta
	if almostEqual(before, 0) {
		if delta > 0 {
			return []changePart{{ActionOpen, DirectionLong, delta}}
		}
		return []changePart{{ActionOpen, DirectionShort, delta}}
	}
	if before > 0 && delta > 0 {
		return []changePart{{ActionAdd, DirectionLong, delta}}
	}
	if before < 0 && delta < 0 {
		return []changePart{{ActionAdd, DirectionShort, delta}}
	}
	if before > 0 {
		if after > 0 {
			return []changePart{{ActionTrim, DirectionLong, delta}}
		}
		if almostEqual(after, 0) {
			return []changePart{{ActionExit, DirectionLong, -before}}
		}
		return []changePart{{ActionExit, DirectionLong, -before}, {ActionOpen, DirectionShort, after}}
	}
	if after < 0 {
		return []changePart{{ActionTrim, DirectionShort, delta}}
	}
	if almostEqual(after, 0) {
		return []changePart{{ActionExit, DirectionShort, -before}}
	}
	return []changePart{{ActionExit, DirectionShort, -before}, {ActionOpen, DirectionLong, after}}
}

func buildChange(group groupedTrade, part changePart, before float64, input Input, index scoringIndex, fxRates []flexstmt.FXRate, pathReason string) Change {
	ratio := math.Abs(part.delta / group.delta)
	cost := group.directCostsBase
	if cost != nil {
		value := *cost * ratio
		cost = &value
	}
	change := Change{ConID: group.conid, Symbol: group.symbol, AssetClass: group.assetClass, Currency: group.currency, Action: part.action, Direction: part.direction, ExecutedAt: group.executedAt, DeltaQuantity: part.delta, PositionBefore: before, PositionAfter: before + part.delta, ExecutionVWAP: cloneFloat(group.vwap), Multiplier: cloneFloat(group.multiplier), DirectCostsBase: cost,
		ID: opaqueID("change", group.key, part.action, part.direction, strconv.FormatFloat(part.delta, 'g', -1, 64))}
	for _, sessions := range Horizons {
		change.Scores = append(change.Scores, scoreHorizon(change, group, sessions, input, index, fxRates, pathReason))
	}
	return change
}

func scoreHorizon(change Change, group groupedTrade, sessions int, input Input, index scoringIndex, fxRates []flexstmt.FXRate, pathReason string) HorizonScore {
	score := HorizonScore{Sessions: sessions}
	if pathReason != "" {
		score.Reason = pathReason
		return score
	}
	if !eligibleAsset(group.assetClass) {
		score.Reason = ReasonUnsupportedAsset
		return score
	}
	if !group.queryComplete {
		score.Reason = ReasonQueryFieldMissing
		return score
	}
	if group.costFXMissing {
		score.Reason = ReasonMissingFX
		return score
	}
	if group.vwap == nil || *group.vwap <= 0 || group.multiplier == nil || *group.multiplier <= 0 || group.directCostsBase == nil || almostEqual(group.delta, 0) {
		score.Reason = ReasonQueryFieldMissing
		return score
	}
	if !index.rawBarsByConID[group.conid] {
		score.Reason = ReasonMarketDataUnavailable
		return score
	}
	bar, ok := horizonBar(index.barsByConID[group.conid], group.executedAt, sessions)
	if !ok {
		score.Reason = ReasonMissingHorizon
		return score
	}
	day, close := bar.Day, bar.Close
	score.HorizonDay, score.HorizonClose = &day, &close
	if reason := interveningReason(group, bar.Day, index.mutationsByConID[group.conid]); reason != "" {
		score.Reason = reason
		return score
	}
	fx := horizonFX(group.currency, input.BaseCurrency, bar.Day, fxRates)
	if fx == nil {
		score.Reason = ReasonMissingFX
		return score
	}
	score.HorizonFX = fx
	notional := math.Abs(change.DeltaQuantity) * *group.multiplier * *group.vwap * *fx
	score.DecisionNotionalBase = &notional
	impact := change.DeltaQuantity * *group.multiplier * (bar.Close - *group.vwap) * *fx
	impact -= *change.DirectCostsBase
	score.DecisionImpactBase = &impact
	impactPct := impact / notional * 100
	score.DecisionImpactPct = &impactPct
	score.MarketContext = buildMarketContext(index.contextBars, group.executedAt, bar.Day)
	return score
}

func buildMarketContext(series map[string][]DailyBar, executedAt, horizonDay time.Time) []MarketContext {
	out := make([]MarketContext, 0, len(marketBenchmarks))
	for _, benchmark := range marketBenchmarks {
		bars := series[benchmark.Symbol]
		start, startOK := contextStartBar(bars, executedAt)
		end, endOK := contextEndBar(bars, horizonDay)
		if !startOK || !endOK || start.Close <= 0 || end.Close <= 0 {
			continue
		}
		changePct := (end.Close/start.Close - 1) * 100
		row := MarketContext{
			Key: benchmark.Key, Label: benchmark.Label, Kind: benchmark.Kind,
			StartDay: start.Day, EndDay: end.Day, StartClose: start.Close, EndClose: end.Close,
			ChangePct: changePct,
		}
		if benchmark.Kind == MarketContextKindVolatility {
			points := end.Close - start.Close
			row.ChangePoints = &points
		}
		out = append(out, row)
	}
	return out
}

// contextStartBar deliberately uses the last close before the execution
// session. It cannot look through an intraday decision to that session's close.
func contextStartBar(bars []DailyBar, executedAt time.Time) (DailyBar, bool) {
	first := sort.Search(len(bars), func(i int) bool { return !bars[i].Day.Before(dayStart(executedAt)) })
	if first == 0 {
		return DailyBar{}, false
	}
	bar := bars[first-1]
	if dayStart(executedAt).Sub(dayStart(bar.Day)) > 7*24*time.Hour {
		return DailyBar{}, false
	}
	return bar, true
}

func contextEndBar(bars []DailyBar, horizonDay time.Time) (DailyBar, bool) {
	target := dayKey(horizonDay)
	index := sort.Search(len(bars), func(i int) bool { return dayKey(bars[i].Day) >= target })
	if index >= len(bars) || dayKey(bars[index].Day) != target {
		return DailyBar{}, false
	}
	return bars[index], true
}

func normalizedBars(in []DailyBar, through time.Time) []DailyBar {
	byDay := map[string]DailyBar{}
	for _, bar := range in {
		if bar.Close <= 0 || bar.Day.After(endOfDay(through)) {
			continue
		}
		key := dayKey(bar.Day)
		current, ok := byDay[key]
		if !ok || bar.ConID != 0 && current.ConID == 0 {
			byDay[key] = bar
		}
	}
	keys := make([]string, 0, len(byDay))
	for key := range byDay {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]DailyBar, 0, len(keys))
	for _, key := range keys {
		out = append(out, byDay[key])
	}
	return out
}

func horizonBar(bars []DailyBar, after time.Time, sessions int) (DailyBar, bool) {
	first := sort.Search(len(bars), func(i int) bool { return bars[i].Day.After(dayStart(after)) })
	index := first + sessions - 1
	if sessions < 1 || index < 0 || index >= len(bars) {
		return DailyBar{}, false
	}
	return bars[index], true
}

// interveningReason receives the already sorted timeline for group.conid.
// Binary searches skip unrelated history instead of rescanning every account
// mutation for every change and horizon.
func interveningReason(group groupedTrade, horizon time.Time, mutations []mutation) string {
	dayFrom, dayTo := dayStart(group.executedAt), endOfDay(group.executedAt)
	firstSameDay := sort.Search(len(mutations), func(i int) bool { return !mutations[i].at.Before(dayFrom) })
	afterSameDay := sort.Search(len(mutations), func(i int) bool { return mutations[i].at.After(dayTo) })
	for _, item := range mutations[firstSameDay:afterSameDay] {
		if item.key != group.key && item.kind != "trade" {
			return mutationInterveningReason(item)
		}
	}
	firstLater := sort.Search(len(mutations), func(i int) bool { return mutations[i].at.After(group.executedAt) })
	for _, item := range mutations[firstLater:] {
		if item.at.After(endOfDay(horizon)) {
			break
		}
		if item.key != group.key {
			return mutationInterveningReason(item)
		}
	}
	return ""
}

func mutationInterveningReason(item mutation) string {
	if item.kind == "corporate_action" {
		return ReasonCorporateAction
	}
	return ReasonInterveningChange
}

func horizonFX(currency, base string, day time.Time, rates []flexstmt.FXRate) *float64 {
	currency, base = strings.ToUpper(strings.TrimSpace(currency)), strings.ToUpper(strings.TrimSpace(base))
	if currency != "" && currency == base {
		value := 1.0
		return &value
	}
	var candidate flexstmt.FXRate
	for _, rate := range rates {
		if rate.Rate == nil || *rate.Rate <= 0 || rate.Date.After(endOfDay(day)) {
			continue
		}
		direct := rate.FromCurrency == currency && (base == "" || rate.ToCurrency == base)
		inverse := rate.ToCurrency == currency && (base == "" || rate.FromCurrency == base)
		if !direct && !inverse {
			continue
		}
		if candidate.Date.IsZero() || rate.Date.After(candidate.Date) {
			candidate = rate
			if inverse {
				value := 1 / *rate.Rate
				candidate.Rate = &value
			}
		}
	}
	if candidate.Rate == nil || day.Sub(dayStart(candidate.Date)) > 7*24*time.Hour {
		return nil
	}
	return cloneFloat(candidate.Rate)
}

func baseConversionFX(currency, base string, day time.Time, stated *float64, rates []flexstmt.FXRate) *float64 {
	currency, base = strings.ToUpper(strings.TrimSpace(currency)), strings.ToUpper(strings.TrimSpace(base))
	if currency != "" && base != "" && currency == base {
		value := 1.0
		return &value
	}
	if stated != nil && *stated > 0 {
		return cloneFloat(stated)
	}
	return horizonFX(currency, base, day, rates)
}

func buildRollups(changes []Change) []ActionRollup {
	actions := []string{ActionOpen, ActionAdd, ActionTrim, ActionExit}
	out := make([]ActionRollup, 0, len(actions))
	for _, action := range actions {
		row := ActionRollup{Action: action}
		for _, sessions := range Horizons {
			values := []float64{}
			contextValues := make(map[string][]MarketContext)
			for _, change := range changes {
				if change.Action == action {
					if score := scoreFor(change, sessions); score != nil && score.DecisionImpactBase != nil {
						values = append(values, *score.DecisionImpactBase)
						for _, context := range score.MarketContext {
							contextValues[context.Key] = append(contextValues[context.Key], context)
						}
					}
				}
			}
			h := HorizonRollup{Sessions: sessions, SampleCount: len(values)}
			if len(values) > 0 {
				total := 0.0
				for _, value := range values {
					total += value
				}
				sort.Float64s(values)
				median := values[len(values)/2]
				if len(values)%2 == 0 {
					median = (values[len(values)/2-1] + values[len(values)/2]) / 2
				}
				h.TotalBase, h.MedianBase = &total, &median
			}
			h.MarketContext = buildMarketContextRollups(contextValues)
			row.Horizons = append(row.Horizons, h)
		}
		out = append(out, row)
	}
	return out
}

func buildMarketContextRollups(values map[string][]MarketContext) []MarketContextRollup {
	out := make([]MarketContextRollup, 0, len(marketBenchmarks))
	for _, benchmark := range marketBenchmarks {
		rows := values[benchmark.Key]
		if len(rows) == 0 {
			continue
		}
		percentages := make([]float64, 0, len(rows))
		points := make([]float64, 0, len(rows))
		for _, row := range rows {
			percentages = append(percentages, row.ChangePct)
			if row.ChangePoints != nil {
				points = append(points, *row.ChangePoints)
			}
		}
		rollup := MarketContextRollup{Key: benchmark.Key, Label: benchmark.Label, Kind: benchmark.Kind, SampleCount: len(rows)}
		medianPct := median(percentages)
		rollup.MedianChangePct = &medianPct
		if len(points) > 0 {
			medianPoints := median(points)
			rollup.MedianChangePoints = &medianPoints
		}
		out = append(out, rollup)
	}
	return out
}

func buildFindings(changes []Change, startingEquity float64) []Finding {
	// Store the strongest observations for every horizon; RPC selects a lens
	// without ranking again.
	out := []Finding{}
	if startingEquity <= 0 {
		return out
	}
	minimumNotional := startingEquity * MinimumFindingNotionalEquityPct / 100
	minimumImpact := startingEquity * MinimumFindingImpactEquityPct / 100
	for _, sessions := range Horizons {
		candidates := []Finding{}
		for _, change := range changes {
			score := scoreFor(change, sessions)
			if score == nil || score.DecisionNotionalBase == nil || score.DecisionImpactBase == nil || score.DecisionImpactPct == nil {
				continue
			}
			if *score.DecisionNotionalBase < minimumNotional || math.Abs(*score.DecisionImpactBase) < minimumImpact {
				continue
			}
			candidates = append(candidates, Finding{
				ChangeID: change.ID, Symbol: change.Symbol, Action: change.Action, Direction: change.Direction,
				ExecutedAt: change.ExecutedAt, HorizonSessions: sessions,
				DecisionNotionalBase: *score.DecisionNotionalBase,
				DecisionImpactBase:   *score.DecisionImpactBase,
				DecisionImpactPct:    *score.DecisionImpactPct,
				MarketContext:        append([]MarketContext(nil), score.MarketContext...),
			})
		}
		sort.Slice(candidates, func(i, j int) bool {
			ai, aj := math.Abs(candidates[i].DecisionImpactPct), math.Abs(candidates[j].DecisionImpactPct)
			if !almostEqual(ai, aj) {
				return ai > aj
			}
			bi, bj := math.Abs(candidates[i].DecisionImpactBase), math.Abs(candidates[j].DecisionImpactBase)
			if !almostEqual(bi, bj) {
				return bi > bj
			}
			return candidates[i].ChangeID < candidates[j].ChangeID
		})
		// Keep the largest absolute observation first, then seat the
		// strongest opposite-sign result second when one exists. The default
		// three-row review therefore cannot hide the best loss behind several
		// larger gains (or the best gain behind several larger losses).
		if len(candidates) > 1 {
			firstPositive := candidates[0].DecisionImpactBase >= 0
			for i := 1; i < len(candidates); i++ {
				if (candidates[i].DecisionImpactBase >= 0) == firstPositive {
					continue
				}
				opposite := candidates[i]
				copy(candidates[2:i+1], candidates[1:i])
				candidates[1] = opposite
				break
			}
		}
		if len(candidates) > 3 {
			candidates = candidates[:3]
		}
		out = append(out, candidates...)
	}
	return out
}

func buildOptionReview(ev evidence, from, to time.Time, base string, fxRates []flexstmt.FXRate) OptionReview {
	instruments := make(map[int64]flexstmt.Instrument, len(ev.instruments))
	for _, instrument := range ev.instruments {
		if instrument.ConID != 0 {
			instruments[instrument.ConID] = instrument
		}
	}
	groups := map[string][]flexstmt.Trade{}
	tradeIDs := map[string]bool{}
	for _, trade := range ev.trades {
		if !strings.EqualFold(trade.AssetClass, "OPT") || trade.ConID == 0 || trade.ExecutedAt.Before(from) || trade.ExecutedAt.After(to) {
			continue
		}
		if trade.TradeID != "" {
			tradeIDs[trade.TradeID] = true
		}
		key := "execution:" + trade.RecordID
		if trade.OrderID != "" {
			key = strings.Join([]string{"order", trade.AccountID, trade.OrderID, dayKey(trade.ExecutedAt)}, "\x00")
		}
		groups[key] = append(groups[key], trade)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	review := OptionReview{}
	for _, key := range keys {
		rows := groups[key]
		grouping := OptionGroupingUnlinkedExecution
		if strings.HasPrefix(key, "order\x00") {
			grouping = OptionGroupingExactOrder
		}
		episode := optionEpisodeFromTrades(key, grouping, rows, instruments, base, fxRates)
		review.Coverage.ExecutionEpisodes++
		switch episode.Lifecycle {
		case OptionLifecycleOpening:
			review.Coverage.OpeningEpisodes++
		case OptionLifecycleClosing:
			review.Coverage.ClosingEpisodes++
		case OptionLifecycleMixed:
			review.Coverage.MixedEpisodes++
		default:
			review.Coverage.UnknownEpisodes++
		}
		openingOnlyZero := episode.Lifecycle == OptionLifecycleOpening && episode.PNLStatus == OptionPNLComplete && episode.RealizedPNLBase != nil && almostEqual(*episode.RealizedPNLBase, 0)
		if openingOnlyZero {
			review.Coverage.OpeningOnlyZeroEpisodes++
			continue
		}
		// Opening-only rows without a non-zero broker realized value are
		// lifecycle coverage, not realized outcomes.
		if episode.Lifecycle == OptionLifecycleOpening && (episode.RealizedPNLBase == nil || almostEqual(*episode.RealizedPNLBase, 0)) {
			continue
		}
		review.Realized.Episodes = append(review.Realized.Episodes, episode)
	}
	for _, event := range ev.optionEvents {
		if event.ConID == 0 || event.Date.Before(from) || event.Date.After(to) || event.TradeID != "" && tradeIDs[event.TradeID] {
			continue
		}
		review.Coverage.EventEpisodes++
		review.Realized.Episodes = append(review.Realized.Episodes, optionEpisodeFromEvent(event, instruments, base, fxRates))
	}
	sort.Slice(review.Realized.Episodes, func(i, j int) bool {
		if !review.Realized.Episodes[i].ActivityFrom.Equal(review.Realized.Episodes[j].ActivityFrom) {
			return review.Realized.Episodes[i].ActivityFrom.Before(review.Realized.Episodes[j].ActivityFrom)
		}
		return review.Realized.Episodes[i].ID < review.Realized.Episodes[j].ID
	})
	review.Realized = summarizeOptionRealized(review.Realized.Episodes)

	for _, position := range ev.currentPositions {
		if !strings.EqualFold(position.AssetClass, "OPT") || position.ConID == 0 || position.Quantity == nil || almostEqual(*position.Quantity, 0) {
			continue
		}
		review.Open.Positions = append(review.Open.Positions, optionOpenPosition(position, instruments, base, fxRates))
	}
	sort.Slice(review.Open.Positions, func(i, j int) bool {
		if !review.Open.Positions[i].SnapshotDate.Equal(review.Open.Positions[j].SnapshotDate) {
			return review.Open.Positions[i].SnapshotDate.Before(review.Open.Positions[j].SnapshotDate)
		}
		return review.Open.Positions[i].ID < review.Open.Positions[j].ID
	})
	review.Open = summarizeOptionOpen(review.Open.Positions)
	// The authoritative Open Positions snapshot can be present and empty.
	// Preserve its through-date independently of whether it contains an option
	// row so confirmed zero inventory does not look like missing evidence.
	review.Open.SnapshotDate = ev.latestOpenPositionDate
	return review
}

func optionEpisodeFromTrades(key, grouping string, rows []flexstmt.Trade, instruments map[int64]flexstmt.Instrument, base string, fxRates []flexstmt.FXRate) OptionEpisode {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].ExecutedAt.Equal(rows[j].ExecutedAt) {
			return rows[i].ExecutedAt.Before(rows[j].ExecutedAt)
		}
		return rows[i].RecordID < rows[j].RecordID
	})
	episode := OptionEpisode{ID: opaqueID("option", "episode", key), Grouping: grouping, Lifecycle: optionTradeLifecycle(rows), ActivityFrom: rows[0].ExecutedAt, ActivityTo: rows[len(rows)-1].ExecutedAt}
	legRows := map[string][]flexstmt.Trade{}
	for _, row := range rows {
		if episode.Underlying == "" {
			episode.Underlying = firstNonEmpty(row.UnderlyingSymbol, instruments[row.ConID].UnderlyingSymbol)
		}
		legKey := strings.Join([]string{strconv.FormatInt(row.ConID, 10), strings.ToUpper(row.Side), optionOpenClose(row.OpenClose), strings.ToUpper(row.Currency)}, "\x00")
		legRows[legKey] = append(legRows[legKey], row)
	}
	keys := make([]string, 0, len(legRows))
	for legKey := range legRows {
		keys = append(keys, legKey)
	}
	sort.Strings(keys)
	known, missing := 0, 0
	var total float64
	missingSet := map[string]bool{}
	for _, legKey := range keys {
		leg, legKnown, legMissing := optionEpisodeTradeLeg(legKey, legRows[legKey], instruments, base, fxRates)
		episode.Legs = append(episode.Legs, leg)
		if legKnown {
			known++
			total += *leg.RealizedPNLBase
		}
		if legMissing {
			missing++
		}
		for _, reason := range leg.MissingEvidence {
			missingSet[reason] = true
		}
	}
	episode.RealizedPNLBase, episode.PNLStatus = optionPNLState(total, known, missing)
	episode.MissingEvidence = sortedKeys(missingSet)
	return episode
}

func optionEpisodeTradeLeg(key string, rows []flexstmt.Trade, instruments map[int64]flexstmt.Instrument, base string, fxRates []flexstmt.FXRate) (OptionEpisodeLeg, bool, bool) {
	first := rows[0]
	instrument, instrumentOK := instruments[first.ConID]
	leg := OptionEpisodeLeg{
		ID: opaqueID("option-leg", key), Symbol: first.Symbol,
		Underlying: firstNonEmpty(first.UnderlyingSymbol, instrument.UnderlyingSymbol),
		Expiry:     optionExpiry(instrument.Expiry), Strike: cloneFloat(instrument.Strike), PutCall: optionPutCall(instrument.PutCall),
		Multiplier: cloneFloat(first.Multiplier), Side: optionTradeSide(first.Side), OpenClose: optionOpenClose(first.OpenClose), Currency: strings.ToUpper(first.Currency),
	}
	missingSet := map[string]bool{}
	if !instrumentOK || leg.Expiry == "" || leg.Strike == nil || leg.PutCall == "" {
		missingSet[OptionMissingInstrument] = true
	}
	var quantity, weighted, weight, realized, costs float64
	quantityKnown, realizedKnown, realizedMissing, costsKnown, costsMissing := true, 0, 0, 0, 0
	for _, row := range rows {
		if row.Quantity == nil {
			quantityKnown = false
		} else {
			q := math.Abs(*row.Quantity)
			quantity += q
			if row.Price != nil {
				weighted += q * *row.Price
				weight += q
			}
		}
		fx := baseConversionFX(row.Currency, base, row.ExecutedAt, row.FXRateToBase, fxRates)
		if row.RealizedPNL == nil {
			realizedMissing++
			missingSet[OptionMissingRealizedPNL] = true
		} else if fx == nil {
			realizedMissing++
			missingSet[OptionMissingFX] = true
		} else {
			realized += *row.RealizedPNL * *fx
			realizedKnown++
		}
		if value, ok := optionTradeCostsBase(row, base, fxRates); ok {
			costs += value
			costsKnown++
		} else {
			costsMissing++
		}
	}
	if quantityKnown {
		leg.Quantity = &quantity
	}
	if weight > 0 {
		value := weighted / weight
		leg.ExecutionPrice = &value
	}
	leg.RealizedPNLBase, _ = optionPNLState(realized, realizedKnown, realizedMissing)
	if costsKnown > 0 && costsMissing == 0 {
		leg.DirectCostsBase = &costs
	}
	leg.MissingEvidence = sortedKeys(missingSet)
	return leg, realizedKnown > 0, realizedMissing > 0
}

func optionEpisodeFromEvent(event flexstmt.OptionEvent, instruments map[int64]flexstmt.Instrument, base string, fxRates []flexstmt.FXRate) OptionEpisode {
	instrument, instrumentOK := instruments[event.ConID]
	missingSet := map[string]bool{}
	leg := OptionEpisodeLeg{
		ID: opaqueID("option-leg", "event", event.RecordID), Symbol: event.Symbol,
		Underlying: firstNonEmpty(event.UnderlyingSymbol, instrument.UnderlyingSymbol),
		Expiry:     optionExpiry(instrument.Expiry), Strike: cloneFloat(instrument.Strike), PutCall: optionPutCall(instrument.PutCall),
		Multiplier: cloneFloat(instrument.Multiplier), Side: "unknown", OpenClose: "unknown", Quantity: cloneFloat(event.Quantity), ExecutionPrice: cloneFloat(event.TradePrice), Currency: strings.ToUpper(event.Currency),
	}
	if !instrumentOK || leg.Expiry == "" || leg.Strike == nil || leg.PutCall == "" {
		missingSet[OptionMissingInstrument] = true
	}
	known, missing := 0, 0
	var realized float64
	fx := baseConversionFX(event.Currency, base, event.Date, event.FXRateToBase, fxRates)
	if event.RealizedPNL == nil {
		missing++
		missingSet[OptionMissingRealizedPNL] = true
	} else if fx == nil {
		missing++
		missingSet[OptionMissingFX] = true
	} else {
		realized = *event.RealizedPNL * *fx
		known++
		leg.RealizedPNLBase = &realized
	}
	if event.CommissionTax != nil && fx != nil {
		costs := math.Abs(*event.CommissionTax) * *fx
		leg.DirectCostsBase = &costs
	}
	leg.MissingEvidence = sortedKeys(missingSet)
	value, status := optionPNLState(realized, known, missing)
	return OptionEpisode{
		ID: opaqueID("option", "event", event.RecordID), Grouping: OptionGroupingEvent, Lifecycle: OptionLifecycleEvent,
		EventType: optionEventType(event.TransactionType), Underlying: leg.Underlying, ActivityFrom: event.Date, ActivityTo: event.Date,
		RealizedPNLBase: value, PNLStatus: status, MissingEvidence: sortedKeys(missingSet), Legs: []OptionEpisodeLeg{leg},
	}
}

func optionOpenPosition(position flexstmt.OpenPosition, instruments map[int64]flexstmt.Instrument, base string, fxRates []flexstmt.FXRate) OptionOpenPosition {
	instrument, instrumentOK := instruments[position.ConID]
	row := OptionOpenPosition{
		ID:     opaqueID("option", "open", position.AccountID, strconv.FormatInt(position.ConID, 10), dayKey(position.ReportDate)),
		Symbol: position.Symbol, Underlying: firstNonEmpty(position.UnderlyingSymbol, instrument.UnderlyingSymbol), SnapshotDate: position.ReportDate,
		Expiry: optionExpiry(instrument.Expiry), Strike: cloneFloat(instrument.Strike), PutCall: optionPutCall(instrument.PutCall),
		Multiplier: cloneFloat(position.Multiplier), Side: optionPositionSide(position.Side), Quantity: cloneFloat(position.Quantity), MarkPrice: cloneFloat(position.MarkPrice),
		CostBasisMoney: cloneFloat(position.CostBasisMoney), Currency: strings.ToUpper(position.Currency), PNLStatus: OptionPNLUnavailable,
	}
	if row.Multiplier == nil {
		row.Multiplier = cloneFloat(instrument.Multiplier)
	}
	missingSet := map[string]bool{}
	if !instrumentOK || row.Expiry == "" || row.Strike == nil || row.PutCall == "" {
		missingSet[OptionMissingInstrument] = true
	}
	fx := baseConversionFX(position.Currency, base, position.ReportDate, position.FXRateToBase, fxRates)
	if position.UnrealizedPNL == nil {
		missingSet[OptionMissingOpenPNL] = true
	} else if fx == nil {
		missingSet[OptionMissingFX] = true
	} else {
		value := *position.UnrealizedPNL * *fx
		row.OpenPNLBase = &value
		row.PNLStatus = OptionPNLComplete
	}
	row.MissingEvidence = sortedKeys(missingSet)
	return row
}

func summarizeOptionRealized(episodes []OptionEpisode) OptionRealizedReview {
	out := OptionRealizedReview{Episodes: episodes}
	var known float64
	knownSeen := false
	for _, episode := range episodes {
		switch episode.PNLStatus {
		case OptionPNLComplete:
			out.CompleteCount++
		case OptionPNLPartial:
			out.PartialCount++
		default:
			out.UnavailableCount++
		}
		if episode.RealizedPNLBase == nil {
			continue
		}
		knownSeen = true
		known += *episode.RealizedPNLBase
		switch {
		case *episode.RealizedPNLBase > 0:
			out.PositiveCount++
		case *episode.RealizedPNLBase < 0:
			out.NegativeCount++
		default:
			out.FlatCount++
		}
	}
	if knownSeen {
		out.KnownPNLBase = &known
	}
	return out
}

func summarizeOptionOpen(positions []OptionOpenPosition) OptionOpenReview {
	out := OptionOpenReview{Positions: positions}
	var known float64
	knownSeen := false
	for _, position := range positions {
		if position.SnapshotDate.After(out.SnapshotDate) {
			out.SnapshotDate = position.SnapshotDate
		}
		if position.PNLStatus == OptionPNLComplete {
			out.CompleteCount++
		} else {
			out.UnavailableCount++
		}
		if position.OpenPNLBase == nil {
			continue
		}
		knownSeen = true
		known += *position.OpenPNLBase
		switch {
		case *position.OpenPNLBase > 0:
			out.PositiveCount++
		case *position.OpenPNLBase < 0:
			out.NegativeCount++
		default:
			out.FlatCount++
		}
	}
	if knownSeen {
		out.KnownPNLBase = &known
	}
	return out
}

func optionTradeLifecycle(rows []flexstmt.Trade) string {
	opening, closing, unknown := false, false, false
	for _, row := range rows {
		switch optionOpenClose(row.OpenClose) {
		case OptionLifecycleOpening:
			opening = true
		case OptionLifecycleClosing:
			closing = true
		default:
			unknown = true
		}
	}
	if unknown {
		return OptionLifecycleUnknown
	}
	if opening && closing {
		return OptionLifecycleMixed
	}
	if opening {
		return OptionLifecycleOpening
	}
	if closing {
		return OptionLifecycleClosing
	}
	return OptionLifecycleUnknown
}

func optionOpenClose(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "O", "OPEN", "OPENING":
		return OptionLifecycleOpening
	case "C", "CLOSE", "CLOSING":
		return OptionLifecycleClosing
	default:
		return OptionLifecycleUnknown
	}
}

func optionTradeSide(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "BUY":
		return "buy"
	case "SELL":
		return "sell"
	default:
		return "unknown"
	}
}

func optionPositionSide(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "LONG", "L":
		return "long"
	case "SHORT", "S":
		return "short"
	default:
		return "unknown"
	}
}

func optionPutCall(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "C", "CALL":
		return "call"
	case "P", "PUT":
		return "put"
	default:
		return ""
	}
}

func optionExpiry(value string) string {
	raw := strings.TrimSpace(value)
	for _, layout := range []string{"20060102", "2006-01-02"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.Format(time.DateOnly)
		}
	}
	return ""
}

func optionEventType(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "EXERCISE":
		return "exercise"
	case "ASSIGNMENT":
		return "assignment"
	case "EXPIRATION", "EXPIRE", "EXPIRED":
		return "expiration"
	default:
		return "other"
	}
}

func optionPNLState(total float64, known, missing int) (*float64, string) {
	if known == 0 {
		return nil, OptionPNLUnavailable
	}
	value := total
	if missing > 0 {
		return &value, OptionPNLPartial
	}
	return &value, OptionPNLComplete
}

func optionTradeCostsBase(row flexstmt.Trade, base string, fxRates []flexstmt.FXRate) (float64, bool) {
	if row.Commission == nil || row.Taxes == nil {
		return 0, false
	}
	tradeFX := baseConversionFX(row.Currency, base, row.ExecutedAt, row.FXRateToBase, fxRates)
	commissionCurrency := firstNonEmpty(row.CommissionCurrency, row.Currency)
	commissionFX := baseConversionFX(commissionCurrency, base, row.ExecutedAt, nil, fxRates)
	if strings.EqualFold(commissionCurrency, row.Currency) {
		commissionFX = tradeFX
	}
	if tradeFX == nil || commissionFX == nil {
		return 0, false
	}
	return math.Abs(*row.Commission)**commissionFX + math.Abs(*row.Taxes)**tradeFX, true
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	middle := len(copyValues) / 2
	if len(copyValues)%2 == 0 {
		return (copyValues[middle-1] + copyValues[middle]) / 2
	}
	return copyValues[middle]
}

func populateCoverage(coverage *Coverage, changes []Change) {
	coverage.TradeChanges = len(changes)
	eligible := map[string]bool{}
	reasonSeen := map[string]map[string]bool{}
	for _, change := range changes {
		// Eligibility is about the decision class, not whether a convenient
		// horizon survived. Keeping every stock/ETF change in the denominator
		// makes high-turnover overlap suppression visible instead of quietly
		// shrinking the comparison set to isolated trades.
		if eligibleAsset(change.AssetClass) {
			eligible[change.ID] = true
		}
		for _, score := range change.Scores {
			if score.DecisionImpactBase != nil {
				coverage.ScoredByHorizon[score.Sessions]++
				continue
			}
			if score.Reason != "" {
				if reasonSeen[score.Reason] == nil {
					reasonSeen[score.Reason] = map[string]bool{}
				}
				reasonSeen[score.Reason][change.ID] = true
			}
		}
	}
	coverage.EligibleChanges = len(eligible)
	for reason, ids := range reasonSeen {
		coverage.ReasonCounts[reason] += len(ids)
	}
}

func fingerprint(result Result) (string, error) {
	copyResult := result
	copyResult.Fingerprint = ""
	raw, err := json.Marshal(copyResult)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(FingerprintVersion+"\x00"), raw...))
	return "edge_" + hex.EncodeToString(sum[:16]), nil
}

func opaqueID(domain string, values ...string) string {
	h := sha256.New()
	h.Write([]byte("canary.edge." + domain + ".v1"))
	for _, value := range values {
		h.Write([]byte{0})
		h.Write([]byte(value))
	}
	return domain + "_" + hex.EncodeToString(h.Sum(nil)[:12])
}

func scoreFor(change Change, sessions int) *HorizonScore {
	for i := range change.Scores {
		if change.Scores[i].Sessions == sessions {
			return &change.Scores[i]
		}
	}
	return nil
}
func eligibleAsset(asset string) bool {
	asset = strings.ToUpper(strings.TrimSpace(asset))
	return asset == "STK" || asset == "ETF"
}
func sectionPresent(coverage []flexstmt.SectionCoverage, key string) bool {
	for _, row := range coverage {
		if row.Key == key {
			return row.Present
		}
	}
	return false
}
func dayKey(v time.Time) string { return v.UTC().Format("2006-01-02") }
func dayStart(v time.Time) time.Time {
	y, m, d := v.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
func endOfDay(v time.Time) time.Time { return dayStart(v).Add(24*time.Hour - time.Nanosecond) }
func afterStartThroughEnd(v, start, end time.Time) bool {
	return v.After(endOfDay(start)) && !v.After(endOfDay(end))
}
func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}
func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
