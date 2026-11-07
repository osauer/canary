package daemon

import (
	"strings"
	"testing"
)

// The rate behind a cross-currency order's limit check comes from the pair the
// broker lists, written the way it lists it. These are the HKD and SGD pairs
// IDEALPRO answered for on 2026-11-07, with each currency on either side of the
// order.
func TestOrderFXEvidenceAsksForTheListedPair(t *testing.T) {
	for _, tc := range []struct {
		contract, base, symbol, currency string
		inverted                         bool
	}{
		{"HKD", "EUR", "EUR", "HKD", true},
		{"EUR", "HKD", "EUR", "HKD", false},
		{"HKD", "GBP", "GBP", "HKD", true},
		{"HKD", "AUD", "AUD", "HKD", true},
		{"HKD", "USD", "USD", "HKD", true},
		{"USD", "HKD", "USD", "HKD", false},
		{"HKD", "CAD", "CAD", "HKD", true},
		{"HKD", "JPY", "HKD", "JPY", false},
		{"JPY", "HKD", "HKD", "JPY", true},
		{"SGD", "EUR", "EUR", "SGD", true},
		{"SGD", "GBP", "GBP", "SGD", true},
		{"SGD", "AUD", "AUD", "SGD", true},
		{"SGD", "USD", "USD", "SGD", true},
		{"USD", "SGD", "USD", "SGD", false},
		{"SGD", "JPY", "SGD", "JPY", false},
		{"SGD", "HKD", "SGD", "HKD", false},
		{"HKD", "SGD", "SGD", "HKD", true},
		{"EUR", "USD", "EUR", "USD", false},
		{"USD", "EUR", "EUR", "USD", true},
	} {
		got, inverted, err := orderFXEvidenceContract(tc.contract, tc.base)
		if err != nil {
			t.Errorf("%s order, %s base: %v", tc.contract, tc.base, err)
			continue
		}
		if got.Symbol != tc.symbol || got.Currency != tc.currency || inverted != tc.inverted || got.SecType != "CASH" || got.Exchange != "IDEALPRO" {
			t.Errorf("%s order, %s base: %+v inverted %v, want %s.%s inverted %v", tc.contract, tc.base, got, inverted, tc.symbol, tc.currency, tc.inverted)
		}
	}
}

// IBKR answers these with code 200, so the preview refuses before asking, and
// says why, rather than reading as a quote that did not come.
func TestOrderFXEvidenceRefusesAPairTheBrokerDoesNotList(t *testing.T) {
	for _, pair := range [][2]string{{"HKD", "NZD"}, {"HKD", "CHF"}, {"SGD", "NZD"}, {"SGD", "CAD"}, {"SGD", "CHF"}, {"HKD", "SEK"}} {
		for _, order := range [][2]string{pair, {pair[1], pair[0]}} {
			if got, _, err := orderFXEvidenceContract(order[0], order[1]); err == nil {
				t.Errorf("%s order, %s base read %s.%s", order[0], order[1], got.Symbol, got.Currency)
			} else if !strings.Contains(err.Error(), "no currency pair") {
				t.Errorf("%s order, %s base: %v", order[0], order[1], err)
			}
		}
	}
}

// A rate may be read for HKD and SGD, but what Canary converts stays as it was:
// the cash engine's pairs, and the row saying a currency has one, are unchanged.
func TestReadingAnAsianPairWidensNoConversion(t *testing.T) {
	for _, currency := range []string{"HKD", "SGD"} {
		if _, _, err := canonicalOrderFXContract(currency, "USD"); err == nil {
			t.Errorf("%s became a pair the cash engine converts", currency)
		}
		if _, listed := orderFXBasePriority[currency]; listed {
			t.Errorf("%s shows as a currency with a conversion pair", currency)
		}
	}
}
