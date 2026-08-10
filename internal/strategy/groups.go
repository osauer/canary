// Package strategy reconstructs conservative option-strategy groups from
// exact current position identities.
package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// InferPositionStrategies performs the deliberately narrow first-pass
// reconstruction used when no Canary or broker combo lineage is available.
// Two exact option legs under one underlying have one decomposition. Larger
// sets remain standalone until a stronger authority groups them.
func InferPositionStrategies(options []rpc.PositionView) ([]rpc.PositionStrategy, []rpc.StrategyGroupingIssue) {
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

	underlyings := make([]string, 0, len(byUnderlying))
	for underlying := range byUnderlying {
		underlyings = append(underlyings, underlying)
	}
	slices.Sort(underlyings)

	strategies := make([]rpc.PositionStrategy, 0, len(underlyings))
	issues := make([]rpc.StrategyGroupingIssue, 0)
	for _, underlying := range underlyings {
		rows := byUnderlying[underlying]
		if len(rows) == 1 {
			continue
		}
		if len(rows) != 2 {
			issues = append(issues, rpc.StrategyGroupingIssue{
				Underlying: underlying,
				LegCount:   len(rows),
				Reason:     "multiple strategy decompositions are possible",
			})
			continue
		}
		strategy, err := inferTwoLegStrategy(underlying, rows)
		if err != nil {
			issues = append(issues, rpc.StrategyGroupingIssue{
				Underlying: underlying,
				LegCount:   len(rows),
				Reason:     err.Error(),
			})
			continue
		}
		strategies = append(strategies, strategy)
	}
	return strategies, issues
}

func inferTwoLegStrategy(underlying string, rows []rpc.PositionView) (rpc.PositionStrategy, error) {
	legs := make([]rpc.PositionStrategyLeg, 0, len(rows))
	quantities := make([]int, 0, len(rows))
	for _, row := range rows {
		if !strings.EqualFold(strings.TrimSpace(row.SecType), "OPT") {
			return rpc.PositionStrategy{}, fmt.Errorf("only exact option contracts can be reconstructed")
		}
		if row.ConID <= 0 {
			return rpc.PositionStrategy{}, fmt.Errorf("an option leg has no exact contract identity")
		}
		quantity, ok := wholeContractQuantity(row.Quantity)
		if !ok || quantity == 0 {
			return rpc.PositionStrategy{}, fmt.Errorf("an option leg does not have a whole contract quantity")
		}
		quantities = append(quantities, quantity)
		legs = append(legs, rpc.PositionStrategyLeg{
			Contract: positionContract(row),
			Quantity: row.Quantity,
		})
	}

	units := gcd(absInt(quantities[0]), absInt(quantities[1]))
	if units <= 0 {
		return rpc.PositionStrategy{}, fmt.Errorf("strategy units cannot be derived")
	}
	for i := range legs {
		legs[i].Ratio = quantities[i] / units
	}
	slices.SortStableFunc(legs, func(a, b rpc.PositionStrategyLeg) int {
		return a.Contract.ConID - b.Contract.ConID
	})

	fingerprint := strategyPositionFingerprint(underlying, legs)
	identity := sha256.Sum256([]byte(strategyIdentityKey(underlying, legs)))
	return rpc.PositionStrategy{
		ID:                  "strategy-" + hex.EncodeToString(identity[:6]),
		Revision:            1,
		Underlying:          underlying,
		Kind:                classifyTwoLegStrategy(rows[0], rows[1]),
		Source:              rpc.PositionStrategySourceInferred,
		Status:              rpc.PositionStrategyStatusCurrent,
		Units:               units,
		Legs:                legs,
		PositionFingerprint: fingerprint,
		Actionable:          true,
		Reason:              "guaranteed route is checked during preview",
	}, nil
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

func classifyTwoLegStrategy(a, b rpc.PositionView) string {
	sameRight := strings.EqualFold(a.Right, b.Right)
	sameExpiry := strings.TrimSpace(a.Expiry) == strings.TrimSpace(b.Expiry)
	sameStrike := math.Abs(a.Strike-b.Strike) < 1e-9
	sameDirection := math.Signbit(a.Quantity) == math.Signbit(b.Quantity)
	switch {
	case sameRight && sameExpiry:
		return "vertical"
	case sameRight && sameStrike:
		return "calendar"
	case sameRight:
		return "diagonal"
	case sameExpiry && sameDirection && sameStrike:
		return "straddle"
	case sameExpiry && sameDirection:
		return "strangle"
	case sameExpiry:
		return "risk_reversal"
	default:
		return "two_leg"
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
