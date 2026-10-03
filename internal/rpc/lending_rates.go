package rpc

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

// NormalizeLendingRateSymbols bounds an explicit symbol-level research read.
// This does not establish exact contract identity or lending eligibility.
func NormalizeLendingRateSymbols(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > 100 {
		return nil, errors.New("supply 1-100 US stock symbols")
	}
	valid := regexp.MustCompile(`^[A-Z0-9][A-Z0-9.]{0,31}$`)
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range input {
		symbol := strings.ToUpper(strings.TrimSpace(raw))
		if !valid.MatchString(symbol) {
			return nil, errors.New("invalid US stock symbol")
		}
		if !seen[symbol] {
			out = append(out, symbol)
			seen[symbol] = true
		}
	}
	slices.Sort(out)
	return out, nil
}

// ValidateLendingRateScope rejects substitution of the requested symbol scope.
// Source statuses and nullable rates remain unchanged for consumer validation.
func ValidateLendingRateScope(result MarketEventsResult, symbols []string) error {
	if !slices.Equal(result.Symbols, symbols) {
		return errors.New("daemon returned a different symbol scope")
	}
	for _, row := range result.BorrowFeeCoverage {
		if !slices.Contains(symbols, row.Symbol) {
			return errors.New("daemon returned out-of-scope fee evidence")
		}
	}
	return nil
}
