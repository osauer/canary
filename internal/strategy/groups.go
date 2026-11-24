// Package strategy reconstructs conservative option-strategy groups from
// exact current position identities.
package strategy

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
)

// InferPositionStrategies groups a book's option legs with no stock beside
// them. See InferStrategies.
func InferPositionStrategies(options []rpc.PositionView) ([]rpc.PositionStrategy, []rpc.StrategyGroupingIssue) {
	return InferStrategies(nil, options)
}

// InferStrategies performs the deliberately narrow reconstruction used when
// no Canary or broker combo lineage is available. The option legs of one
// underlying form a strategy only when the whole set has one reading:
//
//   - two legs have one decomposition, whatever their shape;
//   - three legs must be a butterfly and four an iron condor or iron
//     butterfly, matched exactly (one expiry, even wings, whole ratios);
//   - one short call, or a long put with a short call, written against at
//     least as many shares of the underlying is a covered call or a collar.
//
// Every other set stays standalone with a StrategyGroupingIssue until a
// stronger authority groups it.
func InferStrategies(stocks, options []rpc.PositionView) ([]rpc.PositionStrategy, []rpc.StrategyGroupingIssue) {
	byUnderlying := make(map[string][]rpc.PositionView)
	for _, row := range options {
		if row.Quantity == 0 {
			continue
		}
		underlying := strings.ToUpper(strings.TrimSpace(row.Symbol))
		if underlying == "" {
			continue
		}
		byUnderlying[underlying] = append(byUnderlying[underlying], row)
	}
	shares := longShares(stocks)

	underlyings := make([]string, 0, len(byUnderlying))
	for underlying := range byUnderlying {
		underlyings = append(underlyings, underlying)
	}
	slices.Sort(underlyings)

	strategies := make([]rpc.PositionStrategy, 0, len(underlyings))
	issues := make([]rpc.StrategyGroupingIssue, 0)
	issue := func(underlying string, legs int, reason string) {
		issues = append(issues, rpc.StrategyGroupingIssue{Underlying: underlying, LegCount: legs, Reason: reason})
	}
	for _, underlying := range underlyings {
		rows := byUnderlying[underlying]
		stock, hasStock := shares[underlying]
		if len(rows) == 1 {
			// One option is a position of its own unless it is a call
			// written against the shares.
			if strategy, ok := coveredCall(underlying, rows[0], stock, hasStock); ok {
				strategies = append(strategies, strategy)
			}
			continue
		}
		if sameDirectionStack(rows) {
			continue
		}
		if len(rows) > 2 && !knownShape(rows) {
			issue(underlying, len(rows), "multiple strategy decompositions are possible")
			continue
		}
		strategy, legs, err := inferStrategy(underlying, rows)
		if err != nil {
			issue(underlying, len(rows), err.Error())
			continue
		}
		if hasStock && collar(legs) && rows[0].Multiplier == rows[1].Multiplier && covers(stock, rows[0], strategy.Units) {
			strategy.Kind, strategy.Name = "collar", "collar"
			strategy.Stock = stockCover(stock, rows[0], strategy.Units)
		}
		strategies = append(strategies, strategy)
	}
	return strategies, issues
}

// longShares lists the long stock position of each symbol. A symbol held on
// two lines has no single position to write against and is left out.
func longShares(stocks []rpc.PositionView) map[string]rpc.PositionView {
	out := make(map[string]rpc.PositionView)
	seen := make(map[string]int)
	for _, row := range stocks {
		switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
		case "STK", "STOCK", "ETF":
		default:
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
		if symbol == "" || row.Quantity == 0 {
			continue
		}
		seen[symbol]++
		if row.ConID > 0 && row.Quantity > 0 && !math.IsNaN(row.Quantity) && !math.IsInf(row.Quantity, 0) {
			out[symbol] = row
		}
	}
	for symbol, lines := range seen {
		if lines != 1 {
			delete(out, symbol)
		}
	}
	return out
}

// covers reports whether the shares cover units of an option of this
// contract size, in the option's currency.
func covers(stock, option rpc.PositionView, units int) bool {
	return units > 0 && option.Multiplier > 0 && strings.EqualFold(strings.TrimSpace(stock.Currency), strings.TrimSpace(option.Currency)) &&
		stock.Quantity+1e-9 >= float64(units*option.Multiplier)
}

func stockCover(stock, option rpc.PositionView, units int) *rpc.PositionStrategyLeg {
	return &rpc.PositionStrategyLeg{
		Contract: rpc.ContractParams{ConID: stock.ConID, Symbol: strings.ToUpper(strings.TrimSpace(stock.Symbol)), SecType: "STK",
			Exchange: strings.ToUpper(strings.TrimSpace(stock.Exchange)), Currency: strings.ToUpper(strings.TrimSpace(stock.Currency))},
		Quantity: float64(units * option.Multiplier),
		Ratio:    option.Multiplier,
	}
}

// coveredCall names one short call written against at least as many shares.
// Its call trades on its own, so the group is a description: no combo order
// carries it and it is not actionable as a unit.
func coveredCall(underlying string, call, stock rpc.PositionView, hasStock bool) (rpc.PositionStrategy, bool) {
	if !hasStock || right(call) != "C" || call.Quantity >= 0 {
		return rpc.PositionStrategy{}, false
	}
	strategy, _, err := inferStrategy(underlying, []rpc.PositionView{call})
	if err != nil || !covers(stock, call, strategy.Units) {
		return rpc.PositionStrategy{}, false
	}
	strategy.Kind, strategy.Name = "covered_call", "covered call"
	strategy.Stock = stockCover(stock, call, strategy.Units)
	strategy.Actionable = false
	strategy.Reason = "the call trades on its own order; the shares it is written against stay"
	return strategy, true
}

// sameDirectionStack reports legs of one right that all point the same way:
// two long calls, three long puts. No leg's payoff depends on another, so the
// set is not a strategy and has exactly one decomposition, each leg on its own.
// Only opposite-signed legs form a spread whose exits must stay together.
func sameDirectionStack(rows []rpc.PositionView) bool {
	if len(rows) < 2 {
		return false
	}
	right := strings.ToUpper(strings.TrimSpace(rows[0].Right))
	short := math.Signbit(rows[0].Quantity)
	for _, row := range rows[1:] {
		if strings.ToUpper(strings.TrimSpace(row.Right)) != right || math.Signbit(row.Quantity) != short {
			return false
		}
	}
	return true
}

// strategyLeg is one held row with its signed ratio per unit.
type strategyLeg struct {
	row   rpc.PositionView
	ratio int
}

func (l strategyLeg) long() bool { return l.ratio > 0 }

// inferStrategy builds the group of rows that are known to have one reading:
// every leg an exact option contract in whole contracts, the units their
// greatest common divisor.
func inferStrategy(underlying string, rows []rpc.PositionView) (rpc.PositionStrategy, []strategyLeg, error) {
	legs := make([]rpc.PositionStrategyLeg, 0, len(rows))
	held := make([]strategyLeg, 0, len(rows))
	units := 0
	for _, row := range rows {
		if kind := strings.ToUpper(strings.TrimSpace(row.SecType)); kind != "OPT" && kind != "OPTION" {
			return rpc.PositionStrategy{}, nil, fmt.Errorf("only exact option contracts can be reconstructed")
		}
		if row.ConID <= 0 {
			return rpc.PositionStrategy{}, nil, fmt.Errorf("an option leg has no exact contract identity")
		}
		quantity, ok := wholeContractQuantity(row.Quantity)
		if !ok || quantity == 0 {
			return rpc.PositionStrategy{}, nil, fmt.Errorf("an option leg does not have a whole contract quantity")
		}
		units = gcd(units, absInt(quantity))
		held = append(held, strategyLeg{row: row, ratio: quantity})
		legs = append(legs, rpc.PositionStrategyLeg{
			Contract: positionContract(row),
			Quantity: row.Quantity,
		})
	}
	if units <= 0 {
		return rpc.PositionStrategy{}, nil, fmt.Errorf("strategy units cannot be derived")
	}
	for i := range legs {
		held[i].ratio /= units
		legs[i].Ratio = held[i].ratio
	}
	slices.SortStableFunc(legs, func(a, b rpc.PositionStrategyLeg) int {
		return a.Contract.ConID - b.Contract.ConID
	})

	kind, name := classify(held)
	fingerprint := strategyPositionFingerprint(underlying, legs)
	conIDs := make([]int, len(legs))
	for i, leg := range legs {
		conIDs[i] = leg.Contract.ConID
	}
	return rpc.PositionStrategy{
		ID:                  StrategyID(underlying, conIDs...),
		Revision:            1,
		Underlying:          underlying,
		Kind:                kind,
		Name:                name,
		Source:              rpc.PositionStrategySourceInferred,
		Status:              rpc.PositionStrategyStatusCurrent,
		Units:               units,
		Legs:                legs,
		PositionFingerprint: fingerprint,
		Actionable:          true,
		Reason:              "guaranteed route is checked during preview",
	}, held, nil
}

func wholeContractQuantity(value float64) (int, bool) {
	rounded := math.Round(value)
	if math.Abs(value-rounded) > 1e-9 || rounded > math.MaxInt || rounded < math.MinInt {
		return 0, false
	}
	return int(rounded), true
}

func positionContract(row rpc.PositionView) rpc.ContractParams {
	return rpc.ContractParams{
		ConID:        row.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(row.Symbol)),
		SecType:      "OPT",
		Exchange:     strings.ToUpper(strings.TrimSpace(row.Exchange)),
		Currency:     strings.ToUpper(strings.TrimSpace(row.Currency)),
		LocalSymbol:  strings.TrimSpace(row.LocalSymbol),
		TradingClass: strings.TrimSpace(row.TradingClass),
		Expiry:       strings.TrimSpace(row.Expiry),
		Strike:       row.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(row.Right)),
		Multiplier:   row.Multiplier,
	}
}

// StrategyID is the ID InferStrategies gives the option legs of one
// underlying, in any order: an order that opens a strategy records its
// lineage under the ID the held group carries after the fill.
func StrategyID(underlying string, conIDs ...int) string {
	legs := make([]rpc.PositionStrategyLeg, 0, len(conIDs))
	for _, id := range slices.Sorted(slices.Values(conIDs)) {
		legs = append(legs, rpc.PositionStrategyLeg{Contract: rpc.ContractParams{ConID: id}})
	}
	identity := sha256.Sum256([]byte(strategyIdentityKey(underlying, legs)))
	return "strategy-" + hex.EncodeToString(identity[:6])
}

func strategyIdentityKey(underlying string, legs []rpc.PositionStrategyLeg) string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(strings.TrimSpace(underlying)))
	for _, leg := range legs {
		fmt.Fprintf(&b, "|%d", leg.Contract.ConID)
	}
	return b.String()
}

func strategyPositionFingerprint(underlying string, legs []rpc.PositionStrategyLeg) string {
	var b strings.Builder
	b.WriteString(strategyIdentityKey(underlying, legs))
	for _, leg := range legs {
		fmt.Fprintf(&b, ":%d:%.10g", leg.Ratio, leg.Quantity)
	}
	digest := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func right(row rpc.PositionView) string {
	return strings.ToUpper(strings.TrimSpace(row.Right))
}

func sameStrike(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// oneExpiry reports rows that all carry the same, known expiry.
func oneExpiry(rows []rpc.PositionView) bool {
	expiry := strings.TrimSpace(rows[0].Expiry)
	if expiry == "" {
		return false
	}
	for _, row := range rows[1:] {
		if strings.TrimSpace(row.Expiry) != expiry {
			return false
		}
	}
	return true
}

// knownShape reports a set of three or four legs with exactly one reading: a
// butterfly or an iron condor. Quantities are compared as held, so a set
// whose ratios are off (a wing one contract short) is not the shape.
func knownShape(rows []rpc.PositionView) bool {
	legs := make([]strategyLeg, 0, len(rows))
	units := 0
	for _, row := range rows {
		quantity, ok := wholeContractQuantity(row.Quantity)
		if !ok || quantity == 0 {
			return false
		}
		units = gcd(units, absInt(quantity))
		legs = append(legs, strategyLeg{row: row, ratio: quantity})
	}
	for i := range legs {
		legs[i].ratio /= units
	}
	switch len(legs) {
	case 3:
		_, ok := butterfly(legs)
		return ok
	case 4:
		_, _, ok := ironCondor(legs)
		return ok
	default:
		return false
	}
}

// classify names the shape (kind) and the held position (name) of legs that
// inferStrategy accepted.
func classify(legs []strategyLeg) (kind, name string) {
	switch len(legs) {
	case 1:
		return "", ""
	case 2:
		return classifyTwoLegStrategy(legs[0], legs[1])
	case 3:
		if name, ok := butterfly(legs); ok {
			return "butterfly", name
		}
	case 4:
		if kind, name, ok := ironCondor(legs); ok {
			return kind, name
		}
	}
	return "multi_leg", ""
}

// butterfly matches three legs of one right and expiry at evenly spaced
// strikes, held 1:-2:1 (long) or -1:2:-1 (short).
func butterfly(legs []strategyLeg) (string, bool) {
	rows := []rpc.PositionView{legs[0].row, legs[1].row, legs[2].row}
	side := right(rows[0])
	if !oneExpiry(rows) || side != "C" && side != "P" || right(rows[1]) != side || right(rows[2]) != side {
		return "", false
	}
	sorted := slices.Clone(legs)
	slices.SortStableFunc(sorted, func(a, b strategyLeg) int { return cmp.Compare(a.row.Strike, b.row.Strike) })
	low, body, high := sorted[0], sorted[1], sorted[2]
	wing := body.row.Strike - low.row.Strike
	if wing <= 1e-9 || !sameStrike(high.row.Strike-body.row.Strike, wing) {
		return "", false
	}
	word := map[string]string{"C": "call", "P": "put"}[side]
	switch {
	case low.ratio == 1 && body.ratio == -2 && high.ratio == 1:
		return "long " + word + " butterfly", true
	case low.ratio == -1 && body.ratio == 2 && high.ratio == -1:
		return "short " + word + " butterfly", true
	}
	return "", false
}

// ironCondor matches a put spread below a call spread of one expiry, one
// contract each per unit: wings bought and body sold (an iron condor, or an
// iron butterfly when the short strikes meet), or the reverse.
func ironCondor(legs []strategyLeg) (kind, name string, ok bool) {
	rows := make([]rpc.PositionView, 0, len(legs))
	var puts, calls []strategyLeg
	for _, leg := range legs {
		rows = append(rows, leg.row)
		switch right(leg.row) {
		case "P":
			puts = append(puts, leg)
		case "C":
			calls = append(calls, leg)
		}
		if absInt(leg.ratio) != 1 {
			return "", "", false
		}
	}
	if !oneExpiry(rows) || len(puts) != 2 || len(calls) != 2 {
		return "", "", false
	}
	byStrike := func(a, b strategyLeg) int { return cmp.Compare(a.row.Strike, b.row.Strike) }
	slices.SortStableFunc(puts, byStrike)
	slices.SortStableFunc(calls, byStrike)
	if puts[1].row.Strike-puts[0].row.Strike <= 1e-9 || calls[1].row.Strike-calls[0].row.Strike <= 1e-9 || puts[1].row.Strike > calls[0].row.Strike+1e-9 {
		return "", "", false
	}
	kind, name = "iron_condor", "iron condor"
	if sameStrike(puts[1].row.Strike, calls[0].row.Strike) {
		kind, name = "iron_butterfly", "iron butterfly"
	}
	switch {
	case puts[0].long() && !puts[1].long() && !calls[0].long() && calls[1].long():
		return kind, name, true
	case !puts[0].long() && puts[1].long() && calls[0].long() && !calls[1].long():
		return kind, "reverse " + name, true
	}
	return "", "", false
}

// collar reports a long put with a short call of one expiry, one for one, the
// put struck no higher than the call: with the shares, a collar.
func collar(legs []strategyLeg) bool {
	if len(legs) != 2 || !oneExpiry([]rpc.PositionView{legs[0].row, legs[1].row}) {
		return false
	}
	put, call := legs[0], legs[1]
	if right(put.row) != "P" {
		put, call = call, put
	}
	return right(put.row) == "P" && right(call.row) == "C" && put.ratio == 1 && call.ratio == -1 && put.row.Strike <= call.row.Strike+1e-9
}

// classifyTwoLegStrategy keeps the shape names clients already read and adds
// the held position's name. A direction is named only where the legs are
// held one for one; an uneven vertical is a ratio spread or a backspread.
func classifyTwoLegStrategy(a, b strategyLeg) (kind, name string) {
	sameRight := right(a.row) == right(b.row)
	sameExpiry := strings.TrimSpace(a.row.Expiry) == strings.TrimSpace(b.row.Expiry)
	sameDirection := a.long() == b.long()
	even := absInt(a.ratio) == 1 && absInt(b.ratio) == 1
	word := map[string]string{"C": "call", "P": "put"}[right(a.row)]
	bought, sold := a, b
	if !a.long() {
		bought, sold = b, a
	}
	switch {
	case sameRight && sameExpiry:
		switch {
		case word == "" || sameDirection:
		case !even && absInt(sold.ratio) > absInt(bought.ratio):
			name = word + " ratio spread"
		case !even:
			name = word + " backspread"
		case (word == "put") == (bought.row.Strike > sold.row.Strike):
			// A put spread long the higher strike, or a call spread long
			// the lower one, pays its premium to open.
			name = map[string]string{"put": "bear put spread", "call": "bull call spread"}[word]
		default:
			name = map[string]string{"put": "bull put spread", "call": "bear call spread"}[word]
		}
		return "vertical", name
	case sameRight:
		kind = "diagonal"
		if sameStrike(a.row.Strike, b.row.Strike) {
			kind = "calendar"
		}
		if word != "" && even && !sameDirection && strings.TrimSpace(a.row.Expiry) != "" && strings.TrimSpace(b.row.Expiry) != "" {
			far := a
			if strings.TrimSpace(b.row.Expiry) > strings.TrimSpace(a.row.Expiry) {
				far = b
			}
			name = word + " " + kind + " spread"
			if !far.long() {
				name = "short " + name
			}
		}
		return kind, name
	case sameExpiry && sameDirection:
		kind = "strangle"
		if sameStrike(a.row.Strike, b.row.Strike) {
			kind = "straddle"
		}
		if absInt(a.ratio) == absInt(b.ratio) {
			name = "long " + kind
			if !a.long() {
				name = "short " + kind
			}
		}
		return kind, name
	case sameExpiry:
		if even && right(bought.row) == "C" && right(sold.row) == "P" {
			name = "bullish risk reversal"
			if sameStrike(a.row.Strike, b.row.Strike) {
				name = "synthetic long"
			}
		} else if even && right(bought.row) == "P" && right(sold.row) == "C" {
			name = "bearish risk reversal"
			if sameStrike(a.row.Strike, b.row.Strike) {
				name = "synthetic short"
			}
		}
		return "risk_reversal", name
	default:
		return "two_leg", ""
	}
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
